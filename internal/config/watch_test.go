package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests pin how the store and its watcher see the files: a write the
// store makes itself is not read back as a hand edit, a hand edit is, and a
// refused operation leaves memory and files alone.

// homeTime is the fixture's file time; every write moves a file past it.
var homeTime = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

type home struct {
	dir, path string
	s         *Store
	logs      *logBuffer
	hooks     int
}

// startHome copies the fixture under testdata/compat into a temp home and
// opens a store on it the way the router does at start.
func startHome(t *testing.T, fixture string, standby bool, prepare func(dir string)) *home {
	t.Helper()
	dir := t.TempDir()
	copyHome(t, filepath.Join("testdata", "compat", fixture), dir)
	if prepare != nil {
		prepare(dir)
	}
	t.Setenv("ROUTER_ENV_FILE", filepath.Join(dir, "env"))
	t.Setenv("ROUTER_CLOUD_ONLY", "")
	oldLog := log.Writer()
	t.Cleanup(func() { log.SetOutput(oldLog) })
	logs := &logBuffer{}
	log.SetOutput(logs)

	path := filepath.Join(dir, "providers.json")
	c, err := startup(path, standby)
	if err != nil {
		t.Fatalf("load %s: %v", fixture, err)
	}
	h := &home{dir: dir, path: path, s: NewStore(c, path), logs: logs}
	h.s.OnProfileChange(func() { h.hooks++ })
	return h
}

// logBuffer collects the log; a goroutine left over from another test may
// still write to it.
type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logBuffer) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Reset()
}

// copyHome copies the fixture with the router's modes and gives every file
// the fixture time.
func copyHome(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o600); err != nil {
			return err
		}
		return os.Chtimes(target, homeTime, homeTime)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// appliedAt is when the store last applied a reload.
func (h *home) appliedAt() time.Time {
	h.s.reloadMu.Lock()
	defer h.s.reloadMu.Unlock()
	return h.s.appliedAt
}

// swapApplied sets the reload stamp and gives the previous one. A test zeroes
// it before a tick and reads it after: a Windows clock can give the reload
// the stamp it already had, so comparing two readings misses it.
func (h *home) swapApplied(at time.Time) time.Time {
	h.s.reloadMu.Lock()
	defer h.s.reloadMu.Unlock()
	old := h.s.appliedAt
	h.s.appliedAt = at
	return old
}

// get is a deep copy of the live config, so a later in-place change shows.
func (h *home) get() Config {
	c := h.s.Get()
	c.Local = c.Local.Clone()
	return c
}

type homeFile struct {
	Sum string
	Mod time.Time
}

// files maps every file under the home to its hash and mtime.
func (h *home) files(t *testing.T) map[string]homeFile {
	t.Helper()
	out := map[string]homeFile{}
	err := filepath.WalkDir(h.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(h.dir, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[filepath.ToSlash(rel)] = homeFile{Sum: hex.EncodeToString(sum[:]), Mod: info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// edit replaces the single from in a home file by hand and moves its mtime
// past any write the store made.
func (h *home) edit(t *testing.T, file, from, to string) {
	t.Helper()
	path := filepath.Join(h.dir, filepath.FromSlash(file))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), from) != 1 {
		t.Fatalf("%s has no single %s", file, from)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), from, to, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	edited := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, edited, edited); err != nil {
		t.Fatal(err)
	}
}

// pollQuiet runs one watcher tick and checks that it reloaded nothing.
func (h *home) pollQuiet(t *testing.T) {
	t.Helper()
	h.tickQuiet(t, nil)
}

// tickQuiet runs one watcher tick behind gate and checks that it neither
// reloaded nor tried to.
func (h *home) tickQuiet(t *testing.T, gate Gate) {
	t.Helper()
	want, hooks := h.get(), h.hooks
	last := h.swapApplied(time.Time{})
	h.logs.Reset()
	h.s.tick(gate)
	if got := h.swapApplied(last); !got.IsZero() {
		t.Errorf("the tick applied a reload at %v", got)
	}
	if logs := h.logs.String(); strings.Contains(logs, "reload") {
		t.Errorf("the tick reloaded:\n%s", logs)
	}
	if f := h.s.ReloadFailures(); len(f) != 0 {
		t.Errorf("reload failures after the tick: %+v", f)
	}
	if got := h.get(); !reflect.DeepEqual(got, want) {
		t.Errorf("the tick changed the config:\n got %+v\nwant %+v", got, want)
	}
	if h.hooks != hooks {
		t.Error("the tick ran the profile hook")
	}
}

// swapModels is the setup with lab/old-coder swapped for lab/qwen-coder-next,
// which leaves the inactive cloud profile to repair.
func (h *home) swapModels() Local {
	l := h.s.Get().Local.Clone()
	var models []Model
	for _, m := range l.Models {
		if m.Key() != "lab/old-coder" {
			models = append(models, m)
		}
	}
	l.Models = append(models, Model{Provider: "lab", Model: "qwen-coder-next"})
	return l
}

// codexWork adds a second codex provider without an auth_id to the fixture.
func codexWork(t *testing.T) func(dir string) {
	return func(dir string) {
		path := filepath.Join(dir, "providers.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		codex := `{"name": "codex", "type": "codex", "base_url": "https://chatgpt.com/backend-api/codex"}`
		if strings.Count(string(data), codex) != 1 {
			t.Fatalf("fixture has no single %s", codex)
		}
		data = []byte(strings.Replace(string(data), codex, codex+",\n    "+strings.Replace(codex, `"codex",`, `"codex-work",`, 1), 1))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, homeTime, homeTime); err != nil {
			t.Fatal(err)
		}
	}
}

// catalogProbes are the listings a refresh of the home fixture reads.
func catalogProbes() map[string]ProviderProbe {
	now := time.Now()
	efforts := []string{"low", "high"}
	return map[string]ProviderProbe{
		"codex": {OK: true, At: now, Models: []CatalogModel{{ID: "gpt-6-sol", Name: "GPT-6 Sol", Efforts: efforts}, {ID: "gpt-6.1-sol", Name: "GPT-6.1 Sol", Efforts: efforts}}},
		"lab":   {OK: true, At: now, Models: []CatalogModel{{ID: "qwen-coder"}, {ID: "qwen-coder-next"}}},
		"down":  {Msg: "HTTP 500", At: now},
	}
}

// Every store operation that writes records its own write, so the next
// watcher tick reloads nothing.
func TestStoreWritesDoNotReload(t *testing.T) {
	rows := []struct {
		name    string
		fixture string
		standby bool
		prepare func(dir string)
		op      func(h *home) error
		writes  bool // the op changes a file
		hook    bool // the op runs the profile hook
	}{
		{name: "ensure profiles, present", fixture: "home", op: func(h *home) error { return h.s.EnsureProfiles() }},
		{name: "ensure profiles, first time", fixture: "legacy", writes: true, op: func(h *home) error { return h.s.EnsureProfiles() }},
		{name: "create profile", fixture: "home", writes: true, op: func(h *home) error { return h.s.CreateProfile("night", true) }},
		{name: "activate profile", fixture: "home", writes: true, hook: true, op: func(h *home) error { return h.s.ActivateProfile("cloud") }},
		{name: "delete profile", fixture: "home", writes: true, op: func(h *home) error { return h.s.DeleteProfile("cloud") }},
		{name: "add model", fixture: "home", writes: true, op: func(h *home) error {
			l := h.s.Get().Local.Clone()
			l.Models = append(l.Models, Model{Provider: "lab", Model: "qwen-coder-next"})
			return h.s.Update(Replace(l, ""))
		}},
		{name: "drop model, repair inactive profile", fixture: "home", writes: true, op: func(h *home) error {
			return h.s.Update(Replace(h.swapModels(), ""))
		}},
		{name: "settings, budget", fixture: "home", writes: true, op: func(h *home) error {
			return h.s.UpdateSettings(func(in *SettingsInput) error {
				in.MaxInputChars = "150000"
				return nil
			})
		}},
		{name: "settings, failover", fixture: "home", writes: true, op: func(h *home) error { return h.s.SetFailover(false) }},
		{name: "pool settings", fixture: "home", writes: true, op: func(h *home) error {
			return h.s.SavePoolSettings("work", PoolSettings{Type: PoolBalance, Failover: true, FirstByteSec: 30, ProbeSec: 15, MaxInputChars: 120000})
		}},
		{name: "pool settings merge", fixture: "home", writes: true, op: func(h *home) error {
			return h.s.UpdatePoolSettings("work", func(old PoolSettings) PoolSettings {
				old.ProbeSec = 60
				return old
			})
		}},
		{name: "catalog refresh", fixture: "home", writes: true, op: func(h *home) error {
			return h.s.UpdateCatalog(h.s.Get().Local, []string{"claude-fable-5-1", "claude-opus-5-5", "claude-sonnet-5"}, nil, catalogProbes(), &fakeGate{active: true})
		}},
		{name: "reload", fixture: "home", op: func(h *home) error { return h.s.Reload() }},
		{name: "migrate", fixture: "legacy", standby: true, writes: true, op: func(h *home) error { return h.s.Migrate() }},
		{name: "save codex ids", fixture: "legacy", standby: true, prepare: codexWork(t), writes: true, op: func(h *home) error { return h.s.SaveCodexIDs() }},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			h := startHome(t, row.fixture, row.standby, row.prepare)
			before := h.files(t)
			if err := row.op(h); err != nil {
				t.Fatalf("op: %v", err)
			}
			if wrote := !reflect.DeepEqual(h.files(t), before); wrote != row.writes {
				t.Errorf("op wrote files: %v, want %v", wrote, row.writes)
			}
			if ran := h.hooks != 0; ran != row.hook {
				t.Errorf("op ran the profile hook: %v, want %v", ran, row.hook)
			}
			h.pollQuiet(t)
		})
	}
}

// A standby slot leaves the files to the active one, and its activation
// catches up: codex ids first, then the file the old slot left, then the
// migrations it skipped at start. The chain as a whole leaves nothing to
// reload.
func TestStandbyActivationChain(t *testing.T) {
	h := startHome(t, "legacy", true, codexWork(t))
	h.edit(t, "providers.json", "sk-test-legacy-0001", "sk-test-legacy-0002")
	files := h.files(t)
	h.tickQuiet(t, &fakeGate{})
	if got := h.files(t); !reflect.DeepEqual(got, files) {
		t.Errorf("a standby tick wrote files:\n got %v\nwant %v", got, files)
	}
	if t.Failed() {
		t.FailNow()
	}

	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"save codex ids", h.s.SaveCodexIDs},
		{"reload", h.s.Reload},
		{"migrate", h.s.Migrate},
		{"ensure profiles", h.s.EnsureProfiles},
	} {
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
	}
	if lab, _ := h.s.Get().Local.Provider("lab"); lab.APIKey != "sk-test-legacy-0002" {
		t.Error("activation lost the old slot's edit")
	}
	// The ids the chain saved are the ones the store serves.
	disk, err := ReadProviders(h.path)
	if err != nil {
		t.Fatal(err)
	}
	onDisk, _ := disk.Provider("codex-work")
	served, _ := h.s.Get().Local.Provider("codex-work")
	if onDisk.AuthID == "" || served.AuthID != onDisk.AuthID {
		t.Errorf("codex-work auth_id: store %q, file %q", served.AuthID, onDisk.AuthID)
	}
	if h.hooks != 0 {
		t.Errorf("activation ran the profile hook %d times", h.hooks)
	}
	// The legacy pools survive the id write: the routing is the one an active
	// start migrates the same file to.
	want, err := os.ReadFile(filepath.Join("testdata", "compat", "golden", "02-ensure-profiles", "legacy", "providers.json.profiles", "default.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(h.path + ".profiles/default.json"); err != nil || !bytes.Equal(got, want) {
		t.Errorf("default profile file after activation (%v):\n%s\nwant\n%s", err, got, want)
	}
	profile, err := json.MarshalIndent(h.s.Get().Local.Profiles["default"], "", "  ")
	if err != nil || !bytes.Equal(append(profile, '\n'), want) {
		t.Errorf("served default profile after activation (%v):\n%s", err, profile)
	}
	h.pollQuiet(t)
}

// A hand edit of any file the store reads is reloaded on the next tick; only
// a changed active profile runs the profile hook.
func TestHandEditsReload(t *testing.T) {
	rows := []struct {
		name, file, from, to string
		log                  string
		hook                 bool
		check                func(c Config) bool
	}{
		{"providers", "providers.json", "sk-test-lab-0001", "sk-test-lab-0002", "providers reloaded", false, func(c Config) bool {
			lab, _ := c.Local.Provider("lab")
			return lab.APIKey == "sk-test-lab-0002"
		}},
		{"active profile file", "providers.json.profiles/default.json", `"first_byte_seconds": 90`, `"first_byte_seconds": 91`, "providers reloaded", false, func(c Config) bool {
			return c.Local.PoolSettings["deep"].FirstByteSec == 91 && c.Local.Profiles["default"].PoolSettings["deep"].FirstByteSec == 91
		}},
		{"inactive profile file", "providers.json.profiles/cloud.json", `"max_input_chars": 200000`, `"max_input_chars": 200001`, "providers reloaded", false, func(c Config) bool {
			return c.Local.ActiveProfile == "default" && c.Local.Profiles["cloud"].PoolSettings["cloud-work"].MaxInputChars == 200001
		}},
		{"active profile pointer", "providers.json.active-profile", `"default"`, `"cloud"`, "providers reloaded", true, func(c Config) bool {
			return c.Local.ActiveProfile == "cloud" && c.Local.PoolSettings["cloud-work"].MaxInputChars == 200000
		}},
		{"env", "env", `ROUTER_LOCAL_BALANCE="3"`, `ROUTER_LOCAL_BALANCE="5"`, "env reloaded", false, func(c Config) bool {
			return c.Balance == 5
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			h := startHome(t, "home", false, nil)
			h.swapApplied(time.Time{})
			h.edit(t, row.file, row.from, row.to)
			h.logs.Reset()
			h.s.Poll()
			if h.appliedAt().IsZero() {
				t.Error("Poll applied no reload")
			}
			if logs := h.logs.String(); !strings.Contains(logs, row.log) {
				t.Errorf("log has no %q:\n%s", row.log, logs)
			}
			if f := h.s.ReloadFailures(); len(f) != 0 {
				t.Errorf("reload failures: %+v", f)
			}
			if !row.check(h.s.Get()) {
				t.Errorf("the edit did not reach the config: %+v", h.s.Get().Local)
			}
			if ran := h.hooks != 0; ran != row.hook {
				t.Errorf("Poll ran the profile hook: %v, want %v", ran, row.hook)
			}
			h.pollQuiet(t)
		})
	}
}

// The watcher and the store's own writes share the file stamps, so a tick
// beside a settings change goes through the store lock; run with -race.
func TestPollBesideStoreWrites(t *testing.T) {
	writes := []struct {
		name string
		op   func(h *home, on bool) error
	}{
		{"settings, failover", func(h *home, on bool) error { return h.s.SetFailover(on) }},
		{"pool settings", func(h *home, on bool) error {
			return h.s.SavePoolSettings("work", PoolSettings{Type: PoolBalance, Failover: on, FirstByteSec: 30, ProbeSec: 15})
		}},
	}
	for _, w := range writes {
		t.Run(w.name, func(t *testing.T) {
			h := startHome(t, "home", false, nil)
			done := make(chan struct{})
			go func() {
				defer close(done)
				for range 50 {
					h.s.Poll()
				}
			}()
			for i := range 50 {
				if err := w.op(h, i%2 == 0); err != nil {
					t.Error(err)
				}
			}
			<-done
			h.pollQuiet(t)
		})
	}
}

// A hand edit that lands while the watcher reloads is picked up on the next
// tick: the watcher stamps the files as it found them before the read. The
// profile hook runs after the read, so an edit made there is such an edit.
func TestEditDuringReloadNotLost(t *testing.T) {
	h := startHome(t, "home", false, nil)
	edited := false
	h.s.OnProfileChange(func() {
		if !edited {
			edited = true
			h.edit(t, "providers.json", "sk-test-lab-0001", "sk-test-lab-0002")
		}
	})
	h.edit(t, "providers.json.active-profile", `"default"`, `"cloud"`)
	h.s.Poll()
	if !edited {
		t.Fatal("the reload did not switch the profile")
	}
	h.s.Poll()
	if lab, _ := h.s.Get().Local.Provider("lab"); lab.APIKey != "sk-test-lab-0002" {
		t.Errorf("the edit made during the reload was lost: lab key %q", lab.APIKey)
	}
	h.pollQuiet(t)
}

// A settings change from the UI that lands while the watcher reloads the env
// file is kept, in memory as in the file. The hook runs where such a change
// lands, just before the reload takes the store lock.
func TestSettingsChangeDuringEnvReloadNotLost(t *testing.T) {
	h := startHome(t, "home", false, nil)
	h.edit(t, "env", "ROUTER_LOCAL_FAILOVER=1", "ROUTER_LOCAL_FAILOVER=0")
	changed := false
	h.s.beforeEnvLock = func() {
		if !changed {
			changed = true
			if err := h.s.SetFailover(true); err != nil {
				t.Error(err)
			}
		}
	}
	h.s.Poll()
	if !changed {
		t.Fatal("the tick did not reload the env file")
	}
	env, err := ReadEnv(filepath.Join(h.dir, "env"))
	if err != nil {
		t.Fatal(err)
	}
	if !h.s.Get().Failover || env["ROUTER_LOCAL_FAILOVER"] != "1" {
		t.Errorf("the change made during the reload was lost: memory failover=%v, file %q",
			h.s.Get().Failover, env["ROUTER_LOCAL_FAILOVER"])
	}
	h.pollQuiet(t)
}

// A refused operation says why and changes neither memory nor any file.
func TestRefusalsWriteNothing(t *testing.T) {
	h := startHome(t, "home", false, nil)
	settings := PoolSettings{Type: PoolBalance, Failover: true, FirstByteSec: 30, ProbeSec: 15}
	rows := []struct {
		name string
		op   func() error
		want string
	}{
		{"pool settings, unknown pool", func() error { return h.s.SavePoolSettings("nope", settings) }, `пул "nope" не найден`},
		{"pool settings, bad type", func() error {
			return h.s.UpdatePoolSettings("work", func(old PoolSettings) PoolSettings {
				old.Type = "weird"
				return old
			})
		}, `тип пула "weird" не поддерживается (есть failover и balance)`},
		{"pool settings, other profile", func() error { return h.s.SavePoolSettings("work", settings, "cloud") }, ErrProfileChanged.Error()},
		{"create profile, exists", func() error { return h.s.CreateProfile("cloud", true) }, `профиль "cloud" уже существует`},
		{"create profile, bad name", func() error { return h.s.CreateProfile("night/1", true) }, `неверное имя профиля "night/1"`},
		{"replace, other profile expected", func() error { return h.s.Update(Replace(h.swapModels(), "cloud")) }, ErrProfileChanged.Error()},
		{"replace, other profile in setup", func() error {
			l := h.swapModels()
			l.ActiveProfile = "cloud"
			return h.s.Update(Replace(l, ""))
		}, ErrProfileChanged.Error()},
		{"replace, route to no pool", func() error {
			l := h.swapModels()
			l.FamilyRoutes["opus"]["high"] = Route{Mode: "pool", Pool: "ghost"}
			return h.s.Update(Replace(l, ""))
		}, `профиль "default": пул "ghost" не существует`},
		{"update, fn returned an error", func() error {
			return h.s.Update(func(l *Local) error {
				l.Models = nil
				return errors.New("stop")
			})
		}, "stop"},
		{"settings, bad budget", func() error {
			return h.s.UpdateSettings(func(in *SettingsInput) error {
				in.MaxInputChars = "-1"
				return nil
			})
		}, "max input chars: нужно целое число >= 0"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			mem, files := h.get(), h.files(t)
			err := row.op()
			if err == nil || err.Error() != row.want {
				t.Errorf("error %v, want %q", err, row.want)
			}
			if got := h.get(); !reflect.DeepEqual(got, mem) {
				t.Errorf("config changed:\n got %+v\nwant %+v", got, mem)
			}
			if got := h.files(t); !reflect.DeepEqual(got, files) {
				t.Errorf("files changed:\n got %v\nwant %v", got, files)
			}
		})
	}
	if h.hooks != 0 {
		t.Error("a refusal ran the profile hook")
	}
	h.pollQuiet(t)
}

// signalGate hands each question the watcher asks to the test, so the test
// waits for a tick instead of sleeping through one.
type signalGate struct {
	active atomic.Bool
	asked  chan struct{}
	done   <-chan struct{}
}

func (g *signalGate) WritesSharedState() bool {
	select {
	case g.asked <- struct{}{}:
	case <-g.done:
	}
	return g.active.Load()
}

// wait returns once the watcher has asked the gate again.
func (g *signalGate) wait(t *testing.T) {
	t.Helper()
	select {
	case <-g.asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher stopped asking the gate")
	}
}

// The watcher asks the gate on every tick and reloads only while it is open,
// so a quiesced slot picks up an edit once it is active again.
func TestWatchPausesWhileGateClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	writeRaw(t, path, "ROUTER_LOCAL_BALANCE=1\n")
	t.Setenv("ROUTER_ENV_FILE", path)
	s := NewStore(Config{Balance: 1}, "")
	writeRaw(t, path, "ROUTER_LOCAL_BALANCE=5\n")
	// The watch sees an edit by its mtime, and Windows' clock can give both
	// writes the same one.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := &signalGate{asked: make(chan struct{}), done: ctx.Done()}
	s.Watch(ctx, time.Millisecond, g)

	// The second question starts only after the first tick has finished.
	g.wait(t)
	g.wait(t)
	if got := s.Get().Balance; got != 1 {
		t.Fatalf("the watcher reloaded behind a closed gate: balance %d", got)
	}
	g.active.Store(true)
	g.wait(t)
	g.wait(t)
	if got := s.Get().Balance; got != 5 {
		t.Fatalf("the watcher did not resume behind an open gate: balance %d", got)
	}
}
