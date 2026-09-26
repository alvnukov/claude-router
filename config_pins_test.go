package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	conf "localrouter/internal/config"
)

// These tests pin how the config store and its watcher behave before the store
// moves into internal/config: a write the store makes itself is not read back
// as a hand edit, a hand edit is, and a refused operation leaves memory and
// files alone. They call only exported store methods, so they survive the
// move unchanged until they move themselves.

// configPinTime is the fixture's file time; every write moves a file past it.
var configPinTime = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

// configPinProbe is the file name the appliedAt probe records a failure under.
const configPinProbe = "pin-probe"

type configPin struct {
	dir, path string
	srv       *routerServer
	cs        *configStore
	logs      *configPinLog
	probes    int
}

// configPinStart copies the fixture under internal/config/testdata/compat
// into a temp home and starts a router on it the way main does, with the
// network stubbed.
func configPinStart(t *testing.T, fixture string, standby bool, prepare func(dir string)) *configPin {
	t.Helper()
	dir := t.TempDir()
	configPinCopy(t, filepath.Join("internal", "config", "testdata", "compat", fixture), dir)
	if prepare != nil {
		prepare(dir)
	}
	for _, key := range []string{"ROUTER_UPSTREAM_URL", "ROUTER_LISTEN", "ROUTER_PUBLIC_LISTEN", "ROUTER_UI_LISTEN", "ROUTER_UI_HISTORY",
		"ROUTER_LOCAL_MAX_INPUT_CHARS", "ROUTER_LOCAL_FAILOVER", "ROUTER_LOCAL_FIRST_BYTE_TIMEOUT", "ROUTER_LOCAL_BALANCE", "ROUTER_LOCAL_PROBE_INTERVAL",
		"ROUTER_STANDBY", "ROUTER_SLOT", "ROUTER_ACTIVE_SLOT_FILE", "ROUTER_CLOUD_ONLY", "ROUTER_UI_HISTORY_FILE", "ROUTER_ANTHROPIC_LIMITS_FILE"} {
		t.Setenv(key, "")
	}
	t.Setenv("HOME", dir)
	path, envPath := filepath.Join(dir, "providers.json"), filepath.Join(dir, "env")
	t.Setenv("ROUTER_PROVIDERS_FILE", path)
	t.Setenv("ROUTER_ENV_FILE", envPath)
	t.Setenv("ROUTER_MODELS_STATE", filepath.Join(dir, "models.json"))
	if standby {
		t.Setenv("ROUTER_STANDBY", "1")
	}
	vals, err := conf.ReadEnv(envPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for k, v := range vals {
		t.Setenv(k, v)
	}

	oldAuth, oldTransport, oldLog := codexAuth, http.DefaultTransport, log.Writer()
	t.Cleanup(func() {
		codexAuth, http.DefaultTransport = oldAuth, oldTransport
		log.SetOutput(oldLog)
	})
	codexAuth = &codexAuthStore{loaded: true, credential: usageCredential("pin-account")}
	http.DefaultTransport = configPinTransport(func(r *http.Request) (*http.Response, error) {
		reply := func(code int, body string) (*http.Response, error) {
			return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		}
		switch r.URL.Host + r.URL.Path {
		case "chatgpt.com/backend-api/codex/models":
			return reply(http.StatusOK, configPinCodexModels)
		case "lab.test/v1/models":
			return reply(http.StatusOK, `{"object":"list","data":[{"id":"qwen-coder","object":"model"},{"id":"qwen-coder-next","object":"model"}]}`)
		case "down.test/v1/models":
			return reply(http.StatusInternalServerError, `{"error":"down"}`)
		}
		t.Errorf("unexpected request to %s", r.URL.Redacted())
		return nil, fmt.Errorf("unexpected request to %s", r.URL.Redacted())
	})
	logs := &configPinLog{}
	log.SetOutput(logs)

	c, err := loadConfigChecked()
	if err != nil {
		t.Fatalf("load %s: %v", fixture, err)
	}
	srv := newRouterServer(c, newLifecycle(standby), filepath.Join(dir, "state.json"))
	srv.ui.fetchAnthropic = func(context.Context) ([]string, error) {
		return []string{"claude-fable-5-1", "claude-opus-5-5", "claude-sonnet-5"}, nil
	}
	return &configPin{dir: dir, path: path, srv: srv, cs: srv.cs, logs: logs}
}

const configPinCodexModels = `{"models":[
{"slug":"gpt-6-sol","display_name":"GPT-6 Sol","visibility":"list","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]},
{"slug":"gpt-6.1-sol","display_name":"GPT-6.1 Sol","visibility":"list","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]}]}`

type configPinTransport func(*http.Request) (*http.Response, error)

func (f configPinTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// configPinLog collects the log; a goroutine left over from another test may
// still write to it.
type configPinLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *configPinLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *configPinLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *configPinLog) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Reset()
}

// configPinCopy copies the fixture with the router's modes and gives every
// file the fixture time.
func configPinCopy(t *testing.T, from, to string) {
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
		return os.Chtimes(target, configPinTime, configPinTime)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// appliedAt reads when the store last applied a reload. The one exported place
// that shows it is a failure record, which names the snapshot still serving,
// so the probe records a failure under a file of its own.
func (p *configPin) appliedAt(t *testing.T) time.Time {
	t.Helper()
	p.probes++
	p.cs.NoteReload(configPinProbe, fmt.Errorf("probe %d", p.probes))
	for _, f := range p.cs.ReloadFailures() {
		if f.File == configPinProbe {
			return f.Snapshot
		}
	}
	t.Fatal("the probe left no failure record")
	return time.Time{}
}

// failures are the store's reload failures without the probe's.
func (p *configPin) failures() []reloadFailure {
	var out []reloadFailure
	for _, f := range p.cs.ReloadFailures() {
		if f.File != configPinProbe {
			out = append(out, f)
		}
	}
	return out
}

// get is a deep copy of the live config, so a later in-place change shows.
func (p *configPin) get() config {
	c := p.cs.Get()
	c.Local = c.Local.Clone()
	return c
}

type configPinFile struct {
	Sum string
	Mod time.Time
}

// files maps every file under the home to its hash and mtime.
func (p *configPin) files(t *testing.T) map[string]configPinFile {
	t.Helper()
	out := map[string]configPinFile{}
	err := filepath.WalkDir(p.dir, func(path string, d fs.DirEntry, err error) error {
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
		rel, err := filepath.Rel(p.dir, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[filepath.ToSlash(rel)] = configPinFile{Sum: hex.EncodeToString(sum[:]), Mod: info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// seat binds a session, so a profile hook that clears them shows.
func (p *configPin) seat() {
	h := p.srv.health
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions = map[string]sessionBinding{"pin": {Model: "lab/qwen-coder"}}
}

func (p *configPin) seated() bool {
	h := p.srv.health
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions != nil
}

// pollQuiet runs one watcher tick and checks that it reloaded nothing.
func (p *configPin) pollQuiet(t *testing.T) {
	t.Helper()
	want, applied := p.get(), p.appliedAt(t)
	p.logs.Reset()
	p.cs.Poll()
	if got := p.appliedAt(t); !got.Equal(applied) {
		t.Errorf("Poll applied a reload at %v; the last one was at %v", got, applied)
	}
	if logs := p.logs.String(); strings.Contains(logs, "providers reloaded") || strings.Contains(logs, "env reloaded") {
		t.Errorf("Poll reloaded:\n%s", logs)
	}
	if f := p.failures(); len(f) != 0 {
		t.Errorf("reload failures after Poll: %+v", f)
	}
	if got := p.get(); !reflect.DeepEqual(got, want) {
		t.Errorf("Poll changed the config:\n got %+v\nwant %+v", got, want)
	}
}

// configPinModels is the setup with lab/old-coder swapped for
// lab/qwen-coder-next, which leaves the inactive cloud profile to repair.
func configPinModels(p *configPin) localSetup {
	l := p.cs.Get().Local.Clone()
	var models []localModel
	for _, m := range l.Models {
		if m.Key() != "lab/old-coder" {
			models = append(models, m)
		}
	}
	l.Models = append(models, localModel{Provider: "lab", Model: "qwen-coder-next"})
	return l
}

// Every store operation that writes records its own write, so the next
// watcher tick reloads nothing. A standby slot's activation writes through a
// free function first; the chain as a whole must leave nothing to reload.
func TestConfigPinStoreWritesDoNotReload(t *testing.T) {
	codexWork := func(dir string) {
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
		if err := os.Chtimes(path, configPinTime, configPinTime); err != nil {
			t.Fatal(err)
		}
	}
	rows := []struct {
		name    string
		fixture string
		standby bool
		prepare func(dir string)
		op      func(p *configPin) error
		writes  bool // the op changes a file
		hook    bool // the op clears the sessions
	}{
		{name: "ensure profiles, present", fixture: "home", op: func(p *configPin) error { return p.cs.EnsureProfiles() }},
		{name: "ensure profiles, first time", fixture: "legacy", writes: true, op: func(p *configPin) error { return p.cs.EnsureProfiles() }},
		{name: "create profile", fixture: "home", writes: true, op: func(p *configPin) error { return p.cs.CreateProfile("night", true) }},
		{name: "activate profile", fixture: "home", writes: true, hook: true, op: func(p *configPin) error { return p.cs.ActivateProfile("cloud") }},
		{name: "delete profile", fixture: "home", writes: true, op: func(p *configPin) error { return p.cs.DeleteProfile("cloud") }},
		{name: "add model", fixture: "home", writes: true, op: func(p *configPin) error {
			l := p.cs.Get().Local.Clone()
			l.Models = append(l.Models, localModel{Provider: "lab", Model: "qwen-coder-next"})
			return p.cs.Update(conf.Replace(l, ""))
		}},
		{name: "drop model, repair inactive profile", fixture: "home", writes: true, op: func(p *configPin) error {
			return p.cs.Update(conf.Replace(configPinModels(p), ""))
		}},
		{name: "settings, budget", fixture: "home", writes: true, op: func(p *configPin) error {
			return p.cs.UpdateSettings(func(in *conf.SettingsInput) error {
				in.MaxInputChars = "150000"
				return nil
			})
		}},
		{name: "settings, failover", fixture: "home", writes: true, op: func(p *configPin) error { return p.cs.SetFailover(false) }},
		{name: "pool settings", fixture: "home", writes: true, op: func(p *configPin) error {
			return p.cs.SavePoolSettings("work", poolSettings{Type: conf.PoolBalance, Failover: true, FirstByteSec: 30, ProbeSec: 15, MaxInputChars: 120000})
		}},
		{name: "pool settings merge", fixture: "home", writes: true, op: func(p *configPin) error {
			return p.cs.UpdatePoolSettings("work", func(old poolSettings) poolSettings {
				old.ProbeSec = 60
				return old
			})
		}},
		{name: "catalog refresh", fixture: "home", writes: true, op: func(p *configPin) error { return p.srv.ui.refreshModels(context.Background()) }},
		{name: "reload", fixture: "home", op: func(p *configPin) error { return p.cs.Reload() }},
		{name: "migrate", fixture: "legacy", standby: true, writes: true, op: func(p *configPin) error { return p.cs.Migrate() }},
		{name: "save codex ids", fixture: "legacy", standby: true, prepare: codexWork, writes: true, op: func(p *configPin) error { return p.cs.SaveCodexIDs() }},
		{name: "activate standby", fixture: "legacy", standby: true, prepare: codexWork, writes: true, op: func(p *configPin) error {
			if err := p.srv.runtimeAdmin(filepath.Join(p.dir, "state.json")).activate(); err != nil {
				return err
			}
			// The ids the free function saved are the ones the store serves.
			disk, err := conf.ReadProviders(p.path)
			if err != nil {
				return err
			}
			want, _ := disk.Provider("codex-work")
			got, _ := p.cs.Get().Local.Provider("codex-work")
			if want.AuthID == "" || got.AuthID != want.AuthID {
				return fmt.Errorf("codex-work auth_id: store %q, file %q", got.AuthID, want.AuthID)
			}
			return nil
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			p := configPinStart(t, row.fixture, row.standby, row.prepare)
			before := p.files(t)
			p.seat()
			if err := row.op(p); err != nil {
				t.Fatalf("op: %v", err)
			}
			if wrote := !reflect.DeepEqual(p.files(t), before); wrote != row.writes {
				t.Errorf("op wrote files: %v, want %v", wrote, row.writes)
			}
			if cleared := !p.seated(); cleared != row.hook {
				t.Errorf("op cleared the sessions: %v, want %v", cleared, row.hook)
			}
			p.seat()
			p.pollQuiet(t)
			if !p.seated() {
				t.Error("Poll cleared the sessions")
			}
		})
	}
}

// A hand edit of any file the store reads is reloaded on the next tick; only
// a changed active profile clears the sessions.
func TestConfigPinHandEditsReload(t *testing.T) {
	rows := []struct {
		name, file, from, to string
		log                  string
		hook                 bool
		check                func(c config) bool
	}{
		{"providers", "providers.json", "sk-test-lab-0001", "sk-test-lab-0002", "providers reloaded", false, func(c config) bool {
			lab, _ := c.Local.Provider("lab")
			return lab.APIKey == "sk-test-lab-0002"
		}},
		{"active profile file", "providers.json.profiles/default.json", `"first_byte_seconds": 90`, `"first_byte_seconds": 91`, "providers reloaded", false, func(c config) bool {
			return c.Local.PoolSettings["deep"].FirstByteSec == 91 && c.Local.Profiles["default"].PoolSettings["deep"].FirstByteSec == 91
		}},
		{"inactive profile file", "providers.json.profiles/cloud.json", `"max_input_chars": 200000`, `"max_input_chars": 200001`, "providers reloaded", false, func(c config) bool {
			return c.Local.ActiveProfile == "default" && c.Local.Profiles["cloud"].PoolSettings["cloud-work"].MaxInputChars == 200001
		}},
		{"active profile pointer", "providers.json.active-profile", `"default"`, `"cloud"`, "providers reloaded", true, func(c config) bool {
			return c.Local.ActiveProfile == "cloud" && c.Local.PoolSettings["cloud-work"].MaxInputChars == 200000
		}},
		{"env", "env", `ROUTER_LOCAL_BALANCE="3"`, `ROUTER_LOCAL_BALANCE="5"`, "env reloaded", false, func(c config) bool {
			return c.Balance == 5
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			p := configPinStart(t, "home", false, nil)
			applied := p.appliedAt(t)
			path := filepath.Join(p.dir, filepath.FromSlash(row.file))
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(data), row.from) != 1 {
				t.Fatalf("%s has no single %s", row.file, row.from)
			}
			if err := os.WriteFile(path, []byte(strings.Replace(string(data), row.from, row.to, 1)), 0o600); err != nil {
				t.Fatal(err)
			}
			edited := time.Now().Add(time.Hour)
			if err := os.Chtimes(path, edited, edited); err != nil {
				t.Fatal(err)
			}
			p.seat()
			p.logs.Reset()
			p.cs.Poll()
			if got := p.appliedAt(t); !got.After(applied) {
				t.Errorf("Poll applied no reload: applied at %v, before the edit %v", got, applied)
			}
			if logs := p.logs.String(); !strings.Contains(logs, row.log) {
				t.Errorf("log has no %q:\n%s", row.log, logs)
			}
			if f := p.failures(); len(f) != 0 {
				t.Errorf("reload failures: %+v", f)
			}
			if !row.check(p.cs.Get()) {
				t.Errorf("the edit did not reach the config: %+v", p.cs.Get().Local)
			}
			if cleared := !p.seated(); cleared != row.hook {
				t.Errorf("Poll cleared the sessions: %v, want %v", cleared, row.hook)
			}
			p.seat()
			p.pollQuiet(t)
		})
	}
}

// A refused operation says why and changes neither memory nor any file.
func TestConfigPinRefusalsWriteNothing(t *testing.T) {
	p := configPinStart(t, "home", false, nil)
	settings := poolSettings{Type: conf.PoolBalance, Failover: true, FirstByteSec: 30, ProbeSec: 15}
	rows := []struct {
		name string
		op   func() error
		want string
	}{
		{"pool settings, unknown pool", func() error { return p.cs.SavePoolSettings("nope", settings) }, `пул "nope" не найден`},
		{"pool settings, bad type", func() error {
			return p.cs.UpdatePoolSettings("work", func(old poolSettings) poolSettings {
				old.Type = "weird"
				return old
			})
		}, `тип пула "weird" не поддерживается (есть failover и balance)`},
		{"pool settings, other profile", func() error { return p.cs.SavePoolSettings("work", settings, "cloud") }, "активный профиль изменился; обновите страницу"},
		{"create profile, exists", func() error { return p.cs.CreateProfile("cloud", true) }, `профиль "cloud" уже существует`},
		{"create profile, bad name", func() error { return p.cs.CreateProfile("night/1", true) }, `неверное имя профиля "night/1"`},
		{"replace, other profile expected", func() error { return p.cs.Update(conf.Replace(configPinModels(p), "cloud")) }, "активный профиль изменился; обновите страницу"},
		{"replace, other profile in setup", func() error {
			l := configPinModels(p)
			l.ActiveProfile = "cloud"
			return p.cs.Update(conf.Replace(l, ""))
		}, "активный профиль изменился; обновите страницу"},
		{"replace, route to no pool", func() error {
			l := configPinModels(p)
			l.FamilyRoutes["opus"]["high"] = modelRoute{Mode: "pool", Pool: "ghost"}
			return p.cs.Update(conf.Replace(l, ""))
		}, `профиль "default": пул "ghost" не существует`},
		{"update, fn returned an error", func() error {
			return p.cs.Update(func(l *localSetup) error {
				l.Models = nil
				return errors.New("stop")
			})
		}, "stop"},
		{"settings, bad budget", func() error {
			return p.cs.UpdateSettings(func(in *conf.SettingsInput) error {
				in.MaxInputChars = "-1"
				return nil
			})
		}, "max input chars: нужно целое число >= 0"},
	}
	p.seat()
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			mem, files := p.get(), p.files(t)
			err := row.op()
			if err == nil || err.Error() != row.want {
				t.Errorf("error %v, want %q", err, row.want)
			}
			if got := p.get(); !reflect.DeepEqual(got, mem) {
				t.Errorf("config changed:\n got %+v\nwant %+v", got, mem)
			}
			if got := p.files(t); !reflect.DeepEqual(got, files) {
				t.Errorf("files changed:\n got %v\nwant %v", got, files)
			}
		})
	}
	if !p.seated() {
		t.Error("a refusal cleared the sessions")
	}
	p.pollQuiet(t)
}
