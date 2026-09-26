//go:build compatgen

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestConfigCompatGenerate records how the store rewrites its files, so the
// move into internal/config can be checked byte for byte. It copies the
// synthetic fixture in internal/config/testdata/compat, runs ten operations
// on it with the code as it is before the move, and after each one dumps the
// files and a TREE listing (mode, size after normalization, path) into
// golden/NN-op. Regenerate only from the pre-move code:
//
//	go test -tags compatgen -run TestConfigCompatGenerate -count=1 .
func TestConfigCompatGenerate(t *testing.T) {
	src := filepath.Join("internal", "config", "testdata", "compat")
	golden := filepath.Join(src, "golden")
	work := t.TempDir()
	for _, dir := range []string{"home", "legacy"} {
		configCompatCopy(t, filepath.Join(src, dir), filepath.Join(work, dir))
	}
	if err := os.RemoveAll(golden); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"ROUTER_UPSTREAM_URL", "ROUTER_LISTEN", "ROUTER_PUBLIC_LISTEN", "ROUTER_UI_LISTEN", "ROUTER_UI_HISTORY",
		"ROUTER_LOCAL_MAX_INPUT_CHARS", "ROUTER_LOCAL_FAILOVER", "ROUTER_LOCAL_FIRST_BYTE_TIMEOUT", "ROUTER_LOCAL_BALANCE", "ROUTER_LOCAL_PROBE_INTERVAL",
		"ROUTER_STANDBY", "ROUTER_SLOT", "ROUTER_ACTIVE_SLOT_FILE", "ROUTER_CLOUD_ONLY"} {
		t.Setenv(key, "")
	}
	t.Setenv("HOME", work)
	oldAuth, oldTransport := codexAuth, http.DefaultTransport
	t.Cleanup(func() { codexAuth, http.DefaultTransport = oldAuth, oldTransport })
	codexAuth = &codexAuthStore{loaded: true, credential: usageCredential("compat-account")}
	http.DefaultTransport = configCompatTransport(func(r *http.Request) (*http.Response, error) {
		reply := func(code int, body string) (*http.Response, error) {
			return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		}
		switch r.URL.Host + r.URL.Path {
		case "chatgpt.com/backend-api/codex/models":
			return reply(http.StatusOK, configCompatCodexModels)
		case "lab.test/v1/models":
			return reply(http.StatusOK, `{"object":"list","data":[{"id":"qwen-coder","object":"model"},{"id":"qwen-coder-next","object":"model"}]}`)
		case "down.test/v1/models":
			return reply(http.StatusInternalServerError, `{"error":"down"}`)
		}
		t.Errorf("unexpected request to %s", r.URL.Redacted())
		return nil, fmt.Errorf("unexpected request to %s", r.URL.Redacted())
	})

	// Startup: the env file wins over inherited variables, as LoadEnvFile does.
	load := func(dir string) *configStore {
		t.Helper()
		path, envPath := filepath.Join(work, dir, "providers.json"), filepath.Join(work, dir, "env")
		t.Setenv("ROUTER_PROVIDERS_FILE", path)
		t.Setenv("ROUTER_ENV_FILE", envPath)
		vals, err := ReadEnv(envPath)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		for k, v := range vals {
			t.Setenv(k, v)
		}
		c, err := loadConfigChecked()
		if err != nil {
			t.Fatalf("load %s: %v", dir, err)
		}
		return NewStore(c, path)
	}
	step := 0
	dump := func(op string, roots ...string) {
		t.Helper()
		step++
		configCompatDump(t, work, filepath.Join(golden, fmt.Sprintf("%02d-%s", step, op)), roots)
	}
	must := func(op string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}

	legacy := load("legacy")
	home := load("home")
	dump("load", "home", "legacy")

	must("legacy profiles", legacy.EnsureProfiles())
	must("home profiles", home.EnsureProfiles())
	dump("ensure-profiles", "home", "legacy")

	// Dropping lab/old-coder leaves the inactive cloud profile pointing at it.
	l := home.Get().Local.Clone()
	var models []localModel
	for _, m := range l.Models {
		if m.Key() != "lab/old-coder" {
			models = append(models, m)
		}
	}
	l.Models = append(models, localModel{Provider: "lab", Model: "qwen-coder-next"})
	must("update", home.ApplyLocal(l, true))
	dump("update", "home")

	must("pool settings", home.SavePoolSettings("work", poolSettings{Type: PoolBalance, Failover: true, FirstByteSec: 30, ProbeSec: 15, MaxInputChars: 120000}))
	dump("pool-settings", "home")

	must("create profile", home.CreateProfile("night", true))
	dump("create-profile", "home")

	must("activate profile", home.ActivateProfile("night", nil))
	dump("activate-profile", "home")

	u := &uiServer{cs: home, fetchAnthropic: func(context.Context) ([]string, error) {
		return []string{"claude-fable-5-1", "claude-opus-5-5", "claude-sonnet-5"}, nil
	}}
	must("catalog", u.refreshModels(context.Background()))
	dump("catalog", "home")

	in := InputFromConfig(home.Get())
	in.MaxInputChars = "150000"
	must("settings", home.Apply(in, true))
	in = InputFromConfig(home.Get())
	in.Failover = "0"
	must("failover", home.Apply(in, true))
	dump("settings", "home")

	must("delete profile", home.DeleteProfile("cloud"))
	dump("delete-profile", "home")

	must("reload", home.Reload(nil))
	dump("reload", "home")
}

// gpt-6.1-sol is a newer Sol and joins the pools that hold gpt-6-sol, except
// where the template effort is not in its list; gpt-6-luna is another family;
// the hidden slug is skipped.
const configCompatCodexModels = `{"models":[
{"slug":"gpt-6-sol","display_name":"GPT-6 Sol","visibility":"list","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]},
{"slug":"gpt-6.1-sol","display_name":"GPT-6.1 Sol","visibility":"list","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]},
{"slug":"gpt-6-luna","display_name":"GPT-6 Luna","visibility":"list","supported_reasoning_levels":[{"effort":"medium"}]},
{"slug":"gpt-6-internal","display_name":"Internal","visibility":"hide"}]}`

type configCompatTransport func(*http.Request) (*http.Response, error)

func (f configCompatTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// configCompatCopy copies the fixture with the modes the router uses: 0700
// directories, 0600 files.
func configCompatCopy(t *testing.T, from, to string) {
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
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

var configCompatTimes = regexp.MustCompile(`"(checked_at|anthropic_updated|updated_at)": "[^"]*"`)

// configCompatNormalize replaces the catalog times a refresh takes from the
// clock; the fixture's own time and the zero time stay as written.
func configCompatNormalize(data []byte) []byte {
	return configCompatTimes.ReplaceAllFunc(data, func(m []byte) []byte {
		if bytes.HasSuffix(m, []byte(`"2026-09-01T10:00:00Z"`)) || bytes.HasSuffix(m, []byte(`"0001-01-01T00:00:00Z"`)) {
			return m
		}
		key, _, _ := bytes.Cut(m, []byte(":"))
		return append(append([]byte(nil), key...), `: "NOW"`...)
	})
}

// configCompatDump copies every file under roots into dir, normalized, and
// lists each entry as "mode size path" in dir/TREE; a directory's size is "-".
func configCompatDump(t *testing.T, work, dir string, roots []string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var tree strings.Builder
	for _, root := range roots {
		err := filepath.WalkDir(filepath.Join(work, root), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(work, path)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if d.IsDir() {
				fmt.Fprintf(&tree, "%s - %s\n", info.Mode(), filepath.ToSlash(rel))
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			data = configCompatNormalize(data)
			fmt.Fprintf(&tree, "%s %d %s\n", info.Mode(), len(data), filepath.ToSlash(rel))
			out := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			return os.WriteFile(out, data, 0o644)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "TREE"), []byte(tree.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
