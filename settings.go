package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// configStore holds the live config behind a lock so the UI can change it while
// requests are in flight. Every request handler takes a snapshot with get().
//
// Two files back it: env for the router settings (routing rule, budget,
// failover) and providers.json for local providers and models. Both are
// rewritten in place by the UI and re-read on a hand edit.
//
// A model id that was ever a configured local model stays local for the life
// of the process: a Claude Code session launched before the change keeps
// sending the old id, and routing that to Anthropic by surprise is the one
// thing the router exists to prevent.
type configStore struct {
	mu        sync.RWMutex
	c         config
	envPath   string
	provPath  string
	envMtime  time.Time
	provMtime time.Time
}

// envFilePath is ROUTER_ENV_FILE or the env file beside the binary.
func envFilePath() string {
	if p := os.Getenv("ROUTER_ENV_FILE"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "env")
	}
	return "env"
}

// loadEnvFile exports the env file into the process before the config is
// read, so the binary can be started directly (by launchd or by hand) with
// nothing sourcing the file first. The file wins over inherited variables,
// the same as the hot reload does later; a missing file is fine.
func loadEnvFile() {
	p := envFilePath()
	vals, err := readEnv(p)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("env: %s: %v", p, err)
		}
		return
	}
	for k, v := range vals {
		os.Setenv(k, v)
	}
	log.Printf("env: %d vars from %s", len(vals), p)
}

func newConfigStore(c config, provPath string) *configStore {
	p := envFilePath()
	s := &configStore{c: c, envPath: p, provPath: provPath}
	s.envMtime = mtime(p)
	s.provMtime = mtime(provPath)
	return s
}

func mtime(p string) time.Time {
	if st, err := os.Stat(p); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

func (s *configStore) get() config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.c
}

// watch applies hand edits to either file without a restart. Only the
// settings the UI can change are reloaded; addresses and the upstream URL are
// bound at start and still need one. A write from the UI bumps the mtime too;
// that reload is a no-op because the values already match.
func (s *configStore) watch(every time.Duration) {
	go func() {
		for range time.Tick(every) {
			if m := mtime(s.envPath); !m.Equal(s.envMtime) {
				s.envMtime = m
				if changed, err := s.reloadEnv(); err != nil {
					log.Printf("env reload: %v", err)
				} else if changed {
					c := s.get()
					log.Printf("env reloaded: failover=%v first-byte=%s budget=%d cloud-only=%s",
						c.failover, c.firstByte, c.maxInputChars, strings.Join(c.cloudOnly, ","))
				}
			}
			if s.provPath == "" {
				continue
			}
			if m := mtime(s.provPath); !m.Equal(s.provMtime) {
				s.provMtime = m
				l, err := readProviders(s.provPath)
				if err != nil {
					if !os.IsNotExist(err) {
						log.Printf("providers reload: %v", err)
					}
					continue
				}
				if err := s.applyLocal(l, false); err != nil {
					log.Printf("providers reload: %v", err)
				} else {
					log.Printf("providers reloaded: %s", s.get().local.summary())
				}
			}
		}
	}()
}

func (l localSetup) summary() string {
	var ms []string
	for _, m := range l.ordered() {
		ms = append(ms, m.Key())
	}
	return fmt.Sprintf("providers=%d models=%s", len(l.Providers), strings.Join(ms, ","))
}

func (s *configStore) reloadEnv() (bool, error) {
	vals, err := readEnv(s.envPath)
	if err != nil {
		return false, err
	}
	before := s.get()
	in := inputFromConfig(before)
	in.MaxInputChars = pick(vals, "ROUTER_LOCAL_MAX_INPUT_CHARS", in.MaxInputChars)
	in.CloudOnly = strings.Split(pick(vals, "ROUTER_CLOUD_ONLY", strings.Join(in.CloudOnly, ",")), ",")
	in.Failover = pick(vals, "ROUTER_LOCAL_FAILOVER", in.Failover)
	in.FirstByte = pick(vals, "ROUTER_LOCAL_FIRST_BYTE_TIMEOUT", in.FirstByte)
	if err := s.apply(in, false); err != nil {
		return false, err
	}
	after := s.get()
	return before.maxInputChars != after.maxInputChars ||
		strings.Join(before.cloudOnly, ",") != strings.Join(after.cloudOnly, ",") ||
		before.failover != after.failover || before.firstByte != after.firstByte, nil
}

// settingsInput is the env-backed part of the settings, as strings from a form.
type settingsInput struct {
	MaxInputChars string
	CloudOnly     []string
	Failover      string // "1" / "0"
	FirstByte     string // seconds
}

func inputFromConfig(c config) settingsInput {
	fo := "0"
	if c.failover {
		fo = "1"
	}
	return settingsInput{
		MaxInputChars: strconv.Itoa(c.maxInputChars),
		CloudOnly:     append([]string(nil), c.cloudOnly...),
		Failover:      fo,
		FirstByte:     strconv.Itoa(int(c.firstByte / time.Second)),
	}
}

// apply validates and installs the env-backed settings; with write set it
// also rewrites the env file so they survive a restart.
func (s *configStore) apply(in settingsInput, write bool) error {
	budget, err := strconv.Atoi(strings.TrimSpace(in.MaxInputChars))
	if err != nil || budget < 0 {
		return fmt.Errorf("max input chars: нужно целое число >= 0")
	}
	firstByte, err := strconv.Atoi(strings.TrimSpace(in.FirstByte))
	if err != nil || firstByte < 0 {
		return fmt.Errorf("first byte timeout: нужно целое число секунд >= 0")
	}
	fo := strings.TrimSpace(in.Failover)
	failover := fo != "0" && fo != ""
	seen := map[string]bool{}
	var cloud []string
	for _, e := range in.CloudOnly {
		e = strings.TrimSpace(e)
		if e == "" || seen[e] {
			continue
		}
		if strings.ContainsAny(e, ", \t\n\"'") {
			return fmt.Errorf("cloud-only entry %q: без пробелов, запятых и кавычек", e)
		}
		seen[e] = true
		cloud = append(cloud, e)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.c
	next.maxInputChars = budget
	next.cloudOnly = cloud
	next.failover = failover
	next.firstByte = time.Duration(firstByte) * time.Second
	if write {
		foS := "0"
		if failover {
			foS = "1"
		}
		updates := map[string]string{
			"ROUTER_LOCAL_MAX_INPUT_CHARS":    strconv.Itoa(budget),
			"ROUTER_CLOUD_ONLY":               strings.Join(cloud, ","),
			"ROUTER_LOCAL_FAILOVER":           foS,
			"ROUTER_LOCAL_FIRST_BYTE_TIMEOUT": strconv.Itoa(firstByte),
		}
		if err := writeEnv(s.envPath, updates); err != nil {
			return fmt.Errorf("запись %s: %w", s.envPath, err)
		}
		s.envMtime = mtime(s.envPath)
	}
	s.c = next
	return nil
}

// applyLocal validates and installs a providers/models setup; with write set
// it also rewrites providers.json. Model ids that drop out of the setup are
// kept in extraLocal so a running session is never rerouted to the cloud.
func (s *configStore) applyLocal(l localSetup, write bool) error {
	if err := l.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.c
	for _, m := range next.local.Models {
		next.extraLocal = appendUnique(next.extraLocal, m.Model)
	}
	next.local = l
	if write {
		if s.provPath == "" {
			return fmt.Errorf("providers file disabled (ROUTER_PROVIDERS_FILE пуст)")
		}
		if err := writeProviders(s.provPath, l); err != nil {
			return fmt.Errorf("запись %s: %w", s.provPath, err)
		}
		s.provMtime = mtime(s.provPath)
	}
	s.c = next
	return nil
}
