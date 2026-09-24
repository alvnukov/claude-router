package main

import (
	"context"
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
// Two files back it: env for the router settings (budget,
// failover) and providers.json for local providers and models. Both are
// rewritten in place by the UI and re-read on a hand edit.
type configStore struct {
	mu           sync.RWMutex
	c            config
	envPath      string
	provPath     string
	envMtime     time.Time
	provMtime    time.Time
	profileMtime time.Time
	health       *health
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
		if os.Getenv("ROUTER_SLOT") != "" && (k == "ROUTER_LISTEN" || k == "ROUTER_UI_LISTEN" || k == "ROUTER_STANDBY" || k == "ROUTER_SLOT" || k == "ROUTER_ACTIVE_SLOT_FILE") {
			continue
		}
		os.Setenv(k, v)
	}
	log.Printf("env: %d vars from %s", len(vals), p)
}

func newConfigStore(c config, provPath string) *configStore {
	p := envFilePath()
	s := &configStore{c: c, envPath: p, provPath: provPath}
	s.envMtime = mtime(p)
	s.provMtime = mtime(provPath)
	s.profileMtime = profilesMtime(provPath)
	return s
}

func mtime(p string) time.Time {
	if st, err := os.Stat(p); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

func (s *configStore) reloadProviders() error {
	if s.provPath == "" {
		return nil
	}
	setup, err := readProviders(s.provPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.applyLocal(setup, false)
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
func (s *configStore) watch(ctx context.Context, every time.Duration, life *lifecycle) {
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if life.mode() != modeActive {
				continue
			}
			if m := mtime(s.envPath); !m.Equal(s.envMtime) {
				s.envMtime = m
				if changed, err := s.reloadEnv(); err != nil {
					log.Printf("env reload: %v", err)
				} else if changed {
					c := s.get()
					log.Printf("env reloaded: failover=%v first-byte=%s balance=%d probe=%s budget=%d",
						c.failover, c.firstByte, c.balance, c.probeEvery, c.maxInputChars)
				}
			}
			if s.provPath == "" {
				continue
			}
			if m, p := mtime(s.provPath), profilesMtime(s.provPath); !m.Equal(s.provMtime) || !p.Equal(s.profileMtime) {
				if err := s.reloadProfiles(s.health); err != nil {
					if !os.IsNotExist(err) {
						log.Printf("providers reload: %v", err)
					}
				} else {
					s.provMtime, s.profileMtime = mtime(s.provPath), profilesMtime(s.provPath)
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
	in.Failover = pick(vals, "ROUTER_LOCAL_FAILOVER", in.Failover)
	in.FirstByte = pick(vals, "ROUTER_LOCAL_FIRST_BYTE_TIMEOUT", in.FirstByte)
	in.Balance = pick(vals, "ROUTER_LOCAL_BALANCE", in.Balance)
	in.ProbeEvery = pick(vals, "ROUTER_LOCAL_PROBE_INTERVAL", in.ProbeEvery)
	if err := s.apply(in, false); err != nil {
		return false, err
	}
	after := s.get()
	return before.maxInputChars != after.maxInputChars ||
		before.failover != after.failover || before.firstByte != after.firstByte ||
		before.balance != after.balance || before.probeEvery != after.probeEvery, nil
}

// settingsInput is the env-backed part of the settings, as strings from a form.
type settingsInput struct {
	MaxInputChars string
	Failover      string // "1" / "0"
	FirstByte     string // seconds
	Balance       string // models to spread over
	ProbeEvery    string // seconds, 0 off
}

func inputFromConfig(c config) settingsInput {
	fo := "0"
	if c.failover {
		fo = "1"
	}
	return settingsInput{
		MaxInputChars: strconv.Itoa(c.maxInputChars),
		Failover:      fo,
		FirstByte:     strconv.Itoa(int(c.firstByte / time.Second)),
		Balance:       strconv.Itoa(c.balance),
		ProbeEvery:    strconv.Itoa(int(c.probeEvery / time.Second)),
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
	balance, err := strconv.Atoi(strings.TrimSpace(in.Balance))
	if err != nil || balance < 0 {
		return fmt.Errorf("balance: нужно целое число моделей >= 0")
	}
	probeEvery, err := strconv.Atoi(strings.TrimSpace(in.ProbeEvery))
	if err != nil || probeEvery < 0 {
		return fmt.Errorf("probe interval: нужно целое число секунд >= 0")
	}
	fo := strings.TrimSpace(in.Failover)
	failover := fo != "0" && fo != ""

	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.c
	next.maxInputChars = budget
	next.failover = failover
	next.firstByte = time.Duration(firstByte) * time.Second
	next.balance = balance
	next.probeEvery = time.Duration(probeEvery) * time.Second
	if write {
		foS := "0"
		if failover {
			foS = "1"
		}
		updates := map[string]string{
			"ROUTER_LOCAL_MAX_INPUT_CHARS":    strconv.Itoa(budget),
			"ROUTER_LOCAL_FAILOVER":           foS,
			"ROUTER_LOCAL_FIRST_BYTE_TIMEOUT": strconv.Itoa(firstByte),
			"ROUTER_LOCAL_BALANCE":            strconv.Itoa(balance),
			"ROUTER_LOCAL_PROBE_INTERVAL":     strconv.Itoa(probeEvery),
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
// it also rewrites providers.json.
func (s *configStore) applyLocal(l localSetup, write bool, expectedProfile ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyLocalLocked(l, write, expectedProfile...)
}

// applyLocalLocked requires s.mu; reload uses it while holding the lock from
// disk read through installation so activation cannot interleave.
func (s *configStore) applyLocalLocked(l localSetup, write bool, expectedProfile ...string) error {
	next := s.c
	if write && l.Profiles != nil && (l.ActiveProfile != next.local.ActiveProfile || len(expectedProfile) > 0 && expectedProfile[0] != "" && expectedProfile[0] != next.local.ActiveProfile) {
		return fmt.Errorf("активный профиль изменился; обновите страницу")
	}
	if err := l.syncActiveProfile(); err != nil {
		return err
	}
	l.repairInactiveProfiles()
	if err := l.validateProfiles(); err != nil {
		return err
	}
	next.local = l
	if migrated, changed := migratePoolSettings(next); changed {
		l = migrated
		next.local = l
	}
	if err := l.syncActiveProfile(); err != nil {
		return err
	}
	if err := l.validateProfiles(); err != nil {
		return err
	}
	if write {
		if s.provPath == "" {
			return fmt.Errorf("providers file disabled (ROUTER_PROVIDERS_FILE пуст)")
		}
		if err := writeProviders(s.provPath, l); err != nil {
			return fmt.Errorf("запись %s: %w", s.provPath, err)
		}
		s.provMtime = mtime(s.provPath)
		s.profileMtime = profilesMtime(s.provPath)
	}
	s.c = next
	return nil
}
