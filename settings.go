package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
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

	reloadMu   sync.Mutex
	reloadErrs map[string]reloadFailure
	appliedAt  time.Time
}

// reloadFailure is the latest rejected hand edit of one settings file. The
// previous snapshot keeps serving until the file is fixed.
type reloadFailure struct {
	File     string
	Err      string
	At       time.Time
	Snapshot time.Time
}

func (s *configStore) NoteReload(file string, err error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	if err == nil {
		delete(s.reloadErrs, file)
		s.appliedAt = time.Now()
		return
	}
	if s.reloadErrs == nil {
		s.reloadErrs = map[string]reloadFailure{}
	}
	if old, ok := s.reloadErrs[file]; ok && old.Err == err.Error() {
		return
	}
	log.Printf("%s reload: %v", filepath.Base(file), err)
	s.reloadErrs[file] = reloadFailure{File: file, Err: err.Error(), At: time.Now(), Snapshot: s.appliedAt}
}

func (s *configStore) ReloadFailures() []reloadFailure {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	out := make([]reloadFailure, 0, len(s.reloadErrs))
	for _, f := range s.reloadErrs {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}

func (s *configStore) reloadFailing(file string) bool {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	_, ok := s.reloadErrs[file]
	return ok
}

// wroteProviders records the router's own write of the providers file and its
// profiles, so the watcher skips it. While a hand edit stands rejected the
// watcher reads them back once instead: the write replaced the rejected file,
// and the banner goes unless a file the write did not touch is still bad.
func (s *configStore) wroteProviders() {
	s.provMtime = mtime(s.provPath)
	s.wroteProfiles()
}

// wroteProfiles is wroteProviders for a write that leaves the providers file
// alone, such as a profile switch or delete.
func (s *configStore) wroteProfiles() {
	s.profileMtime = profilesMtime(s.provPath)
	if s.reloadFailing(s.provPath) {
		s.provMtime = time.Time{}
	}
}

// wroteEnv is wroteProviders for the env file.
func (s *configStore) wroteEnv() {
	s.envMtime = mtime(s.envPath)
	if s.reloadFailing(s.envPath) {
		s.envMtime = time.Time{}
	}
}

// EnvFilePath is ROUTER_ENV_FILE or the env file beside the binary.
func EnvFilePath() string {
	if p := os.Getenv("ROUTER_ENV_FILE"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "env")
	}
	return "env"
}

// LoadEnvFile exports the env file into the process before the config is
// read, so the binary can be started directly (by launchd or by hand) with
// nothing sourcing the file first. The file wins over inherited variables,
// the same as the hot reload does later; a missing file is fine.
func LoadEnvFile() {
	p := EnvFilePath()
	vals, err := ReadEnv(p)
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

func NewStore(c config, provPath string) *configStore {
	p := EnvFilePath()
	s := &configStore{c: c, envPath: p, provPath: provPath}
	s.envMtime = mtime(p)
	s.provMtime = mtime(provPath)
	s.profileMtime = profilesMtime(provPath)
	s.appliedAt = time.Now()
	return s
}

func mtime(p string) time.Time {
	if st, err := os.Stat(p); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

// Migrate runs the config migrations a standby slot skipped at start.
func (s *configStore) Migrate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.provPath == "" {
		return nil
	}
	c, err := MigrateConfig(s.c, s.provPath)
	if err != nil {
		return err
	}
	s.c = c
	s.provMtime, s.profileMtime = mtime(s.provPath), profilesMtime(s.provPath)
	return nil
}

func (s *configStore) Get() config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.c
}

// Watch applies hand edits to either file without a restart. Only the
// settings the UI can change are reloaded; addresses and the upstream URL are
// bound at start and still need one. A write from the UI bumps the mtime too;
// that reload is a no-op because the values already match.
func (s *configStore) Watch(ctx context.Context, every time.Duration, life *lifecycle) {
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
			s.Poll()
		}
	}()
}

// Poll applies a hand edit of either file. A rejected edit keeps the
// previous snapshot and shows on the settings page until a good reload.
func (s *configStore) Poll() {
	if m := mtime(s.envPath); !m.Equal(s.envMtime) {
		s.envMtime = m
		changed, err := s.ReloadEnv()
		s.NoteReload(s.envPath, err)
		if err == nil && changed {
			c := s.Get()
			log.Printf("env reloaded: failover=%v first-byte=%s balance=%d probe=%s budget=%d",
				c.Failover, c.FirstByte, c.Balance, c.ProbeEvery, c.MaxInputChars)
		}
	}
	if s.provPath == "" {
		return
	}
	if m, p := mtime(s.provPath), profilesMtime(s.provPath); !m.Equal(s.provMtime) || !p.Equal(s.profileMtime) {
		if err := s.Reload(s.health); err != nil {
			if !os.IsNotExist(err) {
				s.NoteReload(s.provPath, err)
			}
		} else {
			s.provMtime, s.profileMtime = mtime(s.provPath), profilesMtime(s.provPath)
			s.NoteReload(s.provPath, nil)
			log.Printf("providers reloaded: %s", s.Get().Local.Summary())
		}
	}
}

func (l localSetup) Summary() string {
	var ms []string
	for _, m := range l.Ordered() {
		ms = append(ms, m.Key())
	}
	return fmt.Sprintf("providers=%d models=%s", len(l.Providers), strings.Join(ms, ","))
}

func (s *configStore) ReloadEnv() (bool, error) {
	vals, err := ReadEnv(s.envPath)
	if err != nil {
		return false, err
	}
	before := s.Get()
	in := InputFromConfig(before)
	in.MaxInputChars = pick(vals, "ROUTER_LOCAL_MAX_INPUT_CHARS", in.MaxInputChars)
	in.Failover = pick(vals, "ROUTER_LOCAL_FAILOVER", in.Failover)
	in.FirstByte = pick(vals, "ROUTER_LOCAL_FIRST_BYTE_TIMEOUT", in.FirstByte)
	in.Balance = pick(vals, "ROUTER_LOCAL_BALANCE", in.Balance)
	in.ProbeEvery = pick(vals, "ROUTER_LOCAL_PROBE_INTERVAL", in.ProbeEvery)
	if err := s.Apply(in, false); err != nil {
		return false, err
	}
	after := s.Get()
	return before.MaxInputChars != after.MaxInputChars ||
		before.Failover != after.Failover || before.FirstByte != after.FirstByte ||
		before.Balance != after.Balance || before.ProbeEvery != after.ProbeEvery, nil
}

// settingsInput is the env-backed part of the settings, as strings from a form.
type settingsInput struct {
	MaxInputChars string
	Failover      string // "1" / "0"
	FirstByte     string // seconds
	Balance       string // models to spread over
	ProbeEvery    string // seconds, 0 off
	Type          string // pool type; "" keeps the stored one
}

func InputFromConfig(c config) settingsInput {
	fo := "0"
	if c.Failover {
		fo = "1"
	}
	return settingsInput{
		MaxInputChars: strconv.Itoa(c.MaxInputChars),
		Failover:      fo,
		FirstByte:     strconv.Itoa(int(c.FirstByte / time.Second)),
		Balance:       strconv.Itoa(c.Balance),
		ProbeEvery:    strconv.Itoa(int(c.ProbeEvery / time.Second)),
	}
}

// Apply validates and installs the env-backed settings; with write set it
// also rewrites the env file so they survive a restart.
func (s *configStore) Apply(in settingsInput, write bool) error {
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
	next.MaxInputChars = budget
	next.Failover = failover
	next.FirstByte = time.Duration(firstByte) * time.Second
	next.Balance = balance
	next.ProbeEvery = time.Duration(probeEvery) * time.Second
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
		if err := WriteEnv(s.envPath, updates); err != nil {
			return fmt.Errorf("запись %s: %w", s.envPath, err)
		}
		s.wroteEnv()
	}
	s.c = next
	return nil
}

// ApplyLocal validates and installs a providers/models setup; with write set
// it also rewrites providers.json.
func (s *configStore) ApplyLocal(l localSetup, write bool, expectedProfile ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyLocalLocked(l, write, expectedProfile...)
}

// applyLocalLocked requires s.mu; reload uses it while holding the lock from
// disk read through installation so activation cannot interleave.
func (s *configStore) applyLocalLocked(l localSetup, write bool, expectedProfile ...string) error {
	next := s.c
	if write && l.Profiles != nil && (l.ActiveProfile != next.Local.ActiveProfile || len(expectedProfile) > 0 && expectedProfile[0] != "" && expectedProfile[0] != next.Local.ActiveProfile) {
		return fmt.Errorf("активный профиль изменился; обновите страницу")
	}
	if err := l.syncActiveProfile(); err != nil {
		return err
	}
	l.repairInactiveProfiles()
	if err := l.validateProfiles(); err != nil {
		return err
	}
	next.Local = l
	if migrated, changed := MigratePoolSettings(next); changed {
		l = migrated
		next.Local = l
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
		if err := WriteProviders(s.provPath, l); err != nil {
			return fmt.Errorf("запись %s: %w", s.provPath, err)
		}
		s.wroteProviders()
	}
	s.c = next
	return nil
}
