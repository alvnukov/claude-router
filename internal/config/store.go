package config

import (
	"context"
	"errors"
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

// Store holds the live config behind a lock so the UI can change it while
// requests are in flight. Every request handler takes a snapshot with get().
//
// Two files back it: env for the router settings (budget,
// failover) and providers.json for local providers and models. Both are
// rewritten in place by the UI and re-read on a hand edit.
type Store struct {
	mu           sync.RWMutex
	c            Config
	envPath      string
	provPath     string
	envMtime     time.Time
	provMtime    time.Time
	profileMtime time.Time

	onProfileChange func() // runs under mu when the active profile changes

	reloadMu   sync.Mutex
	reloadErrs map[string]ReloadFailure
	appliedAt  time.Time
}

// ReloadFailure is the latest rejected hand edit of one settings file. The
// previous snapshot keeps serving until the file is fixed.
type ReloadFailure struct {
	File     string
	Err      string
	At       time.Time
	Snapshot time.Time
}

func (s *Store) NoteReload(file string, err error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	if err == nil {
		delete(s.reloadErrs, file)
		s.appliedAt = time.Now()
		return
	}
	if s.reloadErrs == nil {
		s.reloadErrs = map[string]ReloadFailure{}
	}
	if old, ok := s.reloadErrs[file]; ok && old.Err == err.Error() {
		return
	}
	log.Printf("%s reload: %v", filepath.Base(file), err)
	s.reloadErrs[file] = ReloadFailure{File: file, Err: err.Error(), At: time.Now(), Snapshot: s.appliedAt}
}

func (s *Store) ReloadFailures() []ReloadFailure {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	out := make([]ReloadFailure, 0, len(s.reloadErrs))
	for _, f := range s.reloadErrs {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}

func (s *Store) reloadFailing(file string) bool {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	_, ok := s.reloadErrs[file]
	return ok
}

// written names what a write touched, for commit.
type written int

const (
	kindProviders written = iota // providers.json with its profiles
	kindProfiles                 // the profiles alone: a switch or a delete
	kindEnv                      // the env file
)

// commit records the router's own write so the watcher skips it. While a
// hand edit stands rejected the watcher reads the files back once instead:
// the write replaced the rejected file, and the banner goes unless a file the
// write did not touch is still bad. It requires s.mu.
func (s *Store) commit(kind written) {
	switch kind {
	case kindEnv:
		s.envMtime = mtime(s.envPath)
		if s.reloadFailing(s.envPath) {
			s.envMtime = time.Time{}
		}
		return
	case kindProviders:
		s.provMtime = mtime(s.provPath)
	}
	s.profileMtime = profilesMtime(s.provPath)
	if s.reloadFailing(s.provPath) {
		s.provMtime = time.Time{}
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

// Gate says whether this instance may write shared state; a nil Gate always
// may. The store asks it before a timed reload and a catalog merge.
type Gate interface{ WritesSharedState() bool }

// ErrProfileChanged refuses a form built for a profile that is no longer
// active.
var ErrProfileChanged = errors.New("активный профиль изменился; обновите страницу")

func NewStore(c Config, provPath string) *Store {
	p := EnvFilePath()
	s := &Store{c: c, envPath: p, provPath: provPath}
	s.envMtime = mtime(p)
	s.provMtime = mtime(provPath)
	s.profileMtime = profilesMtime(provPath)
	s.appliedAt = time.Now()
	return s
}

// OnProfileChange registers the one function run, under the store lock, when
// the active profile changes; the router drops its pinned sessions there.
func (s *Store) OnProfileChange(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onProfileChange = fn
}

func mtime(p string) time.Time {
	if st, err := os.Stat(p); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

// Migrate runs the config migrations a standby slot skipped at start.
func (s *Store) Migrate() error {
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
	s.commit(kindProviders)
	return nil
}

// SaveCodexIDs writes the auth_id values the start of an active router would
// have written; a standby slot assigned them only in memory. It reads the
// file, not the snapshot: the old slot may have changed it since.
//
// The id write drops the legacy fields, so, as at start, the migrations run
// right after it on what was read; otherwise the reload that follows would
// find the legacy pools gone and Migrate would have nothing to move.
func (s *Store) SaveCodexIDs() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.provPath == "" {
		return nil
	}
	local, assigned, err := LoadLocal(s.provPath)
	if err != nil || !assigned {
		return err
	}
	if err := persist(s.provPath, local, ".before-codex-ids"); err != nil {
		return err
	}
	c := s.c
	c.Local = local
	_, err = MigrateConfig(c, s.provPath)
	s.commit(kindProviders)
	return err
}

func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.c
}

// Watch applies hand edits to either file without a restart. Only the
// settings the UI can change are reloaded; addresses and the upstream URL are
// bound at start and still need one. A write from the UI bumps the mtime too;
// that reload is a no-op because the values already match.
func (s *Store) Watch(ctx context.Context, every time.Duration, gate Gate) {
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			s.tick(gate)
		}
	}()
}

// tick is one round of Watch. A slot that does not write shared state leaves
// the files to the one that does.
func (s *Store) tick(gate Gate) {
	if gate != nil && !gate.WritesSharedState() {
		return
	}
	s.Poll()
}

// Poll applies a hand edit of either file. A rejected edit keeps the
// previous snapshot and shows on the settings page until a good reload. The
// stamps are read and set under s.mu, as the store's own writes set them; the
// reloads take the lock themselves.
func (s *Store) Poll() {
	if s.envEdited() {
		changed, err := s.reloadEnv()
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
	s.mu.RLock()
	was, wasProfiles := s.provMtime, s.profileMtime
	m, p := mtime(s.provPath), profilesMtime(s.provPath)
	s.mu.RUnlock()
	if m.Equal(was) && p.Equal(wasProfiles) {
		return
	}
	if err := s.Reload(); err != nil {
		if !os.IsNotExist(err) {
			s.NoteReload(s.provPath, err)
		}
		return
	}
	// The stamps are the files as found before the read, so an edit made
	// since is read on the next tick; a write the store made meanwhile has
	// set its own.
	s.mu.Lock()
	if s.provMtime.Equal(was) && s.profileMtime.Equal(wasProfiles) {
		s.provMtime, s.profileMtime = m, p
	}
	s.mu.Unlock()
	s.NoteReload(s.provPath, nil)
	log.Printf("providers reloaded: %s", s.Get().Local.Summary())
}

// envEdited tells whether the env file changed since its stamp and takes the
// new stamp, so a rejected edit is not read again.
func (s *Store) envEdited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := mtime(s.envPath)
	if m.Equal(s.envMtime) {
		return false
	}
	s.envMtime = m
	return true
}

func (l Local) Summary() string {
	var ms []string
	for _, m := range l.Ordered() {
		ms = append(ms, m.Key())
	}
	return fmt.Sprintf("providers=%d models=%s", len(l.Providers), strings.Join(ms, ","))
}

func (s *Store) reloadEnv() (bool, error) {
	vals, err := ReadEnv(s.envPath)
	if err != nil {
		return false, err
	}
	before := s.Get()
	err = s.updateSettings(false, func(in *SettingsInput) error {
		in.MaxInputChars = pick(vals, "ROUTER_LOCAL_MAX_INPUT_CHARS", in.MaxInputChars)
		in.Failover = pick(vals, "ROUTER_LOCAL_FAILOVER", in.Failover)
		in.FirstByte = pick(vals, "ROUTER_LOCAL_FIRST_BYTE_TIMEOUT", in.FirstByte)
		in.Balance = pick(vals, "ROUTER_LOCAL_BALANCE", in.Balance)
		in.ProbeEvery = pick(vals, "ROUTER_LOCAL_PROBE_INTERVAL", in.ProbeEvery)
		return nil
	})
	if err != nil {
		return false, err
	}
	after := s.Get()
	return before.MaxInputChars != after.MaxInputChars ||
		before.Failover != after.Failover || before.FirstByte != after.FirstByte ||
		before.Balance != after.Balance || before.ProbeEvery != after.ProbeEvery, nil
}

// SettingsInput is the env-backed part of the settings, as strings from a form.
type SettingsInput struct {
	MaxInputChars string
	Failover      string // "1" / "0"
	FirstByte     string // seconds
	Balance       string // models to spread over
	ProbeEvery    string // seconds, 0 off
	Type          string // pool type; "" keeps the stored one
}

func inputFromConfig(c Config) SettingsInput {
	fo := "0"
	if c.Failover {
		fo = "1"
	}
	return SettingsInput{
		MaxInputChars: strconv.Itoa(c.MaxInputChars),
		Failover:      fo,
		FirstByte:     strconv.Itoa(int(c.FirstByte / time.Second)),
		Balance:       strconv.Itoa(c.Balance),
		ProbeEvery:    strconv.Itoa(int(c.ProbeEvery / time.Second)),
	}
}

// UpdateSettings changes the env-backed settings: fn edits the current ones as
// form strings under the store lock, then the store checks them, rewrites the
// env file and installs them.
func (s *Store) UpdateSettings(fn func(*SettingsInput) error) error {
	return s.updateSettings(true, fn)
}

// SetFailover switches failover and keeps the other settings as the store
// holds them, not as a page showed them.
func (s *Store) SetFailover(on bool) error {
	return s.UpdateSettings(func(in *SettingsInput) error {
		in.Failover = "0"
		if on {
			in.Failover = "1"
		}
		return nil
	})
}

// updateSettings validates and installs the env-backed settings; with write
// set it also rewrites the env file so they survive a restart.
func (s *Store) updateSettings(write bool, fn func(*SettingsInput) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := inputFromConfig(s.c)
	if err := fn(&in); err != nil {
		return err
	}
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
		if err := writeEnv(s.envPath, updates); err != nil {
			return fmt.Errorf("запись %s: %w", s.envPath, err)
		}
		s.commit(kindEnv)
	}
	s.c = next
	return nil
}

// Update changes the providers setup: fn edits a copy of the latest one under
// the store lock, then the store checks the result, rewrites providers.json
// and installs it. A refusal leaves both as they were.
func (s *Store) Update(fn func(*Local) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update("", fn)
}

// Replace is fn for Update that installs next whole, unless the form was
// built for another active profile.
// Помощник форм до 8b-1; правки API — замыканиями Update по полям.
func Replace(next Local, expectedProfile string) func(*Local) error {
	return func(cur *Local) error {
		if next.Profiles != nil && (next.ActiveProfile != cur.ActiveProfile || expectedProfile != "" && expectedProfile != cur.ActiveProfile) {
			return ErrProfileChanged
		}
		*cur = next
		return nil
	}
}

// update is the one way a change of the providers setup reaches the file:
// prepare, write with an optional backup, commit, install. It requires s.mu,
// so a wrapper makes its own checks in the same hold of the lock.
func (s *Store) update(backup string, fn func(*Local) error) error {
	next, err := s.prepare(fn)
	if err != nil {
		return err
	}
	if s.provPath == "" {
		return fmt.Errorf("providers file disabled (ROUTER_PROVIDERS_FILE пуст)")
	}
	if err := persist(s.provPath, next.Local, backup); err != nil {
		return fmt.Errorf("запись %s: %w", s.provPath, err)
	}
	s.commit(kindProviders)
	s.c = next
	return nil
}

// prepare runs fn on a copy of the providers setup and checks the result: the
// active profile takes the edited routing, inactive profiles drop models that
// are gone, pools get their settings. It requires s.mu; a reload installs
// what it returns without a write.
func (s *Store) prepare(fn func(*Local) error) (Config, error) {
	l := s.c.Local.Clone()
	if err := fn(&l); err != nil {
		return Config{}, err
	}
	if err := l.syncActiveProfile(); err != nil {
		return Config{}, err
	}
	l.repairInactiveProfiles()
	if err := l.validateProfiles(); err != nil {
		return Config{}, err
	}
	next := s.c
	next.Local = l
	if migrated, changed := migratePoolSettings(next); changed {
		l = migrated
	}
	if err := l.syncActiveProfile(); err != nil {
		return Config{}, err
	}
	if err := l.validateProfiles(); err != nil {
		return Config{}, err
	}
	next.Local = l
	return next, nil
}
