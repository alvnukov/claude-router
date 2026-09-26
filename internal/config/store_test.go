package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// newTestStore writes a one-provider setup with a single profile to a temp
// providers.json and returns a store over it; the env file sits beside it.
func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ROUTER_ENV_FILE", filepath.Join(dir, "env"))
	path := filepath.Join(dir, "providers.json")
	l := Local{
		Providers:    []Provider{{Name: "lab", BaseURL: "http://lab.test/v1"}},
		Models:       []Model{{Provider: "lab", Model: "coder"}},
		ModelPools:   map[string][]PoolTarget{"work": {{Model: "lab/coder"}}},
		PoolSettings: map[string]PoolSettings{"work": {Type: PoolFailover, Failover: true, FirstByteSec: 20}},
		Routes:       map[string]map[string]Route{"claude-sonnet-5": {"default": {Mode: "pool", Pool: "work"}}},
		FamilyRoutes: map[string]map[string]Route{},
	}
	l.Profiles = map[string]Profile{"default": l.Routing()}
	l.ActiveProfile = "default"
	if err := WriteProviders(path, l); err != nil {
		t.Fatal(err)
	}
	read, err := ReadProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	c := Config{Local: read, MaxInputChars: 100000, Failover: true, FirstByte: 20 * time.Second, Balance: 1, ProbeEvery: 30 * time.Second}
	return NewStore(c, path), path
}

// snapshotFiles reads every file the store writes under dir, keyed by name.
func snapshotFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		out[p] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestConfigRefusesJSON(t *testing.T) {
	for name, v := range map[string]any{"value": Config{}, "field": struct{ C Config }{}} {
		_, err := json.Marshal(v)
		if err == nil || !strings.Contains(err.Error(), "config.Config не кодируется в JSON") {
			t.Errorf("%s: json.Marshal error = %v, want the refusal", name, err)
		}
	}
}

func TestUpdateRefusalLeavesStoreAndFiles(t *testing.T) {
	stop := errors.New("stop")
	for _, tc := range []struct {
		name string
		fn   func(*Local) error
		want error  // matched with errors.Is when set
		text string // substring otherwise
	}{
		{"fn error after a change", func(l *Local) error {
			l.Models = nil
			l.ModelPools["work"] = nil
			return stop
		}, stop, ""},
		{"replace, other active profile", func(l *Local) error {
			next := l.Clone()
			next.Profiles["night"] = next.Routing()
			next.ActiveProfile = "night"
			return Replace(next, "")(l)
		}, ErrProfileChanged, ""},
		{"replace, stale form", func(l *Local) error {
			return Replace(l.Clone(), "night")(l)
		}, ErrProfileChanged, ""},
		{"pipeline rejects", func(l *Local) error {
			l.Models = append(l.Models, Model{Provider: "ghost", Model: "m"})
			return nil
		}, nil, `провайдер "ghost" не существует`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, path := newTestStore(t)
			before, files := s.Get(), snapshotFiles(t, filepath.Dir(path))
			err := s.Update(tc.fn)
			if tc.want != nil && !errors.Is(err, tc.want) || tc.want == nil && (err == nil || !strings.Contains(err.Error(), tc.text)) {
				t.Fatalf("Update error = %v", err)
			}
			if !reflect.DeepEqual(s.Get(), before) {
				t.Error("a refused update changed the snapshot")
			}
			if !reflect.DeepEqual(snapshotFiles(t, filepath.Dir(path)), files) {
				t.Error("a refused update touched the files")
			}
		})
	}
}

func TestUpdateWritesAndSyncsActiveProfile(t *testing.T) {
	s, path := newTestStore(t)
	err := s.Update(func(l *Local) error {
		l.Models = append(l.Models, Model{Provider: "lab", Model: "coder-next"})
		l.ModelPools["work"] = append(l.ModelPools["work"], PoolTarget{Model: "lab/coder-next"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got := s.Get().Local
	if want := []PoolTarget{{Model: "lab/coder"}, {Model: "lab/coder-next"}}; !reflect.DeepEqual(got.Profiles["default"].ModelPools["work"], want) {
		t.Errorf("active profile in memory = %v, want %v", got.Profiles["default"].ModelPools["work"], want)
	}
	read, err := ReadProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	if !read.HasModel("lab/coder-next") || len(read.ModelPools["work"]) != 2 {
		t.Errorf("file after update: models %v, pool %v", read.Models, read.ModelPools["work"])
	}
}

func TestUpdateWithoutProvidersFile(t *testing.T) {
	s, _ := newTestStore(t)
	s.provPath = ""
	err := s.Update(func(*Local) error { return nil })
	if err == nil || err.Error() != "providers file disabled (ROUTER_PROVIDERS_FILE пуст)" {
		t.Fatalf("Update error = %v", err)
	}
	err = s.UpdateCatalog(s.Get().Local, []string{"claude-sonnet-5"}, nil, nil, nil)
	if err == nil || err.Error() != "providers file disabled (ROUTER_PROVIDERS_FILE пуст)" {
		t.Fatalf("UpdateCatalog error = %v", err)
	}
}

func TestSetFailoverKeepsOtherSettings(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.UpdateSettings(func(in *SettingsInput) error {
		in.MaxInputChars = "150000"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFailover(false); err != nil {
		t.Fatal(err)
	}
	c := s.Get()
	if c.MaxInputChars != 150000 || c.Failover || c.FirstByte != 20*time.Second || c.Balance != 1 || c.ProbeEvery != 30*time.Second {
		t.Errorf("settings after failover off: budget=%d failover=%v first-byte=%s balance=%d probe=%s",
			c.MaxInputChars, c.Failover, c.FirstByte, c.Balance, c.ProbeEvery)
	}
	env, err := ReadEnv(s.envPath)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"ROUTER_LOCAL_MAX_INPUT_CHARS":    "150000",
		"ROUTER_LOCAL_FAILOVER":           "0",
		"ROUTER_LOCAL_FIRST_BYTE_TIMEOUT": "20",
		"ROUTER_LOCAL_BALANCE":            "1",
		"ROUTER_LOCAL_PROBE_INTERVAL":     "30",
	}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("env file = %v, want %v", env, want)
	}
}

func TestUpdateSettingsRefusal(t *testing.T) {
	stop := errors.New("stop")
	for _, tc := range []struct {
		name string
		fn   func(*SettingsInput) error
		text string
	}{
		{"fn error", func(in *SettingsInput) error { in.MaxInputChars = "1"; return stop }, "stop"},
		{"bad budget", func(in *SettingsInput) error { in.MaxInputChars = "-1"; return nil }, "max input chars: нужно целое число >= 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestStore(t)
			before := s.Get()
			if err := s.UpdateSettings(tc.fn); err == nil || err.Error() != tc.text {
				t.Fatalf("UpdateSettings error = %v, want %q", err, tc.text)
			}
			if !reflect.DeepEqual(s.Get(), before) {
				t.Error("a refused settings update changed the snapshot")
			}
			if _, err := os.Stat(s.envPath); !os.IsNotExist(err) {
				t.Errorf("a refused settings update wrote the env file: %v", err)
			}
		})
	}
}

func TestProfileChangeHookIsOptional(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.CreateProfile("night", true); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateProfile("night"); err != nil {
		t.Fatalf("activate without a hook: %v", err)
	}
	calls := 0
	s.OnProfileChange(func() { calls++ })
	if err := s.ActivateProfile("default"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || s.Get().Local.ActiveProfile != "default" {
		t.Errorf("hook calls = %d, active = %q", calls, s.Get().Local.ActiveProfile)
	}
}

// fakeGate answers like the lifecycle does: a nil one writes shared state.
type fakeGate struct{ active bool }

func (g *fakeGate) WritesSharedState() bool { return g == nil || g.active }

func TestTickAsksGate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		gate   Gate
		reload bool
	}{
		{"nil gate", nil, true},
		{"typed nil gate", (*fakeGate)(nil), true},
		{"active", &fakeGate{active: true}, true},
		{"standby", &fakeGate{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, path := newTestStore(t)
			l := s.Get().Local.Clone()
			l.Models = append(l.Models, Model{Provider: "lab", Model: "hand-edit"})
			if err := WriteProviders(path, l); err != nil {
				t.Fatal(err)
			}
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(path, later, later); err != nil {
				t.Fatal(err)
			}
			s.tick(tc.gate)
			if got := s.Get().Local.HasModel("lab/hand-edit"); got != tc.reload {
				t.Errorf("hand edit loaded = %v, want %v", got, tc.reload)
			}
		})
	}
}
