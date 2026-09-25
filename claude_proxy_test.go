package main

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRouterEnvPointsClaudeAtTheRouter runs the built binary as the router
// script does, with nothing faked: Claude Code's settings.json gains the
// router's address, other settings stay, and the command prints the address.
func TestRouterEnvPointsClaudeAtTheRouter(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "localrouter")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	home, config := filepath.Join(dir, "home"), filepath.Join(dir, "claude")
	for _, d := range []string{home, config} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	settings := filepath.Join(config, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"env":{"OTHER":"keep"},"model":"opus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "env", "-home", home)
	// The last value of a key wins: no inherited listen address, and no way
	// to reach the real ~/.claude.
	cmd.Env = append(os.Environ(), "ROUTER_LISTEN=", "HOME="+dir, "CLAUDE_CONFIG_DIR="+config)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("localrouter env: %v", err)
	}
	if string(out) != "ANTHROPIC_BASE_URL=http://127.0.0.1:8787\n" {
		t.Fatalf("stdout = %q", out)
	}
	data, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Env   map[string]string
		Model string
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8787" || got.Env["OTHER"] != "keep" || got.Model != "opus" {
		t.Fatalf("settings.json = %s", data)
	}
}

func TestClaudeProxyRestoresEndpointAndPreservesOtherEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := `{"env":{"ANTHROPIC_BASE_URL":"https://previous.example/v1","OTHER":"keep"},"permissions":{"allow":["Read"]},"large":9007199254740993}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	p := &claudeProxy{path: path}
	target := "http://127.0.0.1:8787"
	if p.view(target).Enabled {
		t.Fatal("already enabled")
	}
	if err := p.set(target, true); err != nil {
		t.Fatal(err)
	}
	if err := p.set(target, true); err != nil {
		t.Fatal(err)
	} // double click must not replace backup
	v := p.view(target)
	if !v.Enabled || !v.CanRestore || v.Error != "" {
		t.Fatalf("connected state: %+v", v)
	}
	root, env, _, err := readClaudeSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if proxyURL(env) != target || string(root["large"]) != "9007199254740993" {
		t.Fatal("wrong settings after connect")
	}
	root["newPreference"] = json.RawMessage(`true`)
	env["ADDED"] = json.RawMessage(`"value"`)
	root["env"], _ = json.Marshal(env)
	data, _ := json.Marshal(root)
	writeRaw(t, path, string(data))
	// Simulate a router restart before restoring.
	p = &claudeProxy{path: path}
	if err := p.set(target, false); err != nil {
		t.Fatal(err)
	}
	root, env, _, err = readClaudeSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if proxyURL(env) != "https://previous.example/v1" || string(env["ADDED"]) != `"value"` || string(root["newPreference"]) != "true" || string(root["large"]) != "9007199254740993" {
		t.Fatal("restoring lost original or unrelated edits")
	}
	if _, err := os.Stat(path + ".router-proxy-backup"); !os.IsNotExist(err) {
		t.Fatal("backup not cleared")
	}
	if p.view(target).Enabled {
		t.Fatal("still enabled")
	}
	// A new cycle backs up the current value afresh.
	if err := p.set(target, true); err != nil {
		t.Fatal(err)
	}
	if err := p.set(target, false); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeProxyAbsentAndInvalidFiles(t *testing.T) {
	target := "http://127.0.0.1:8787"
	for _, input := range []string{"missing", `{}`, `{"env":null}`, `{"env":{}}`, `{"env":{"OTHER":"keep"}}`} {
		t.Run(input, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			p := &claudeProxy{path: path}
			if input != "missing" {
				writeRaw(t, path, input)
			}
			if err := p.set(target, true); err != nil {
				t.Fatal(err)
			}
			requirePrivateFile(t, path+".router-proxy-backup")
			if err := p.set(target, false); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if input == "missing" {
				if !os.IsNotExist(err) {
					t.Fatal("created file not removed")
				}
				return
			}
			var got, want any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(input), &want); err != nil {
				t.Fatal(err)
			}
			a, _ := json.Marshal(got)
			b, _ := json.Marshal(want)
			if string(a) != string(b) {
				t.Fatalf("restored %s, want %s", a, b)
			}
		})
	}
	for _, input := range []string{`not JSON`, `null`, `{"env":[]}`} {
		path := filepath.Join(t.TempDir(), "settings.json")
		writeRaw(t, path, input)
		p := &claudeProxy{path: path}
		if err := p.set(target, true); err == nil {
			t.Fatal("invalid file accepted")
		}
		data, _ := os.ReadFile(path)
		if string(data) != input {
			t.Fatal("invalid file overwritten")
		}
	}
}

func TestClaudeProxyDoesNotOverwriteExternalEndpointChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	p := &claudeProxy{path: path}
	target := "http://127.0.0.1:8787"
	if err := p.set(target, true); err != nil {
		t.Fatal(err)
	}
	external := `{"env":{"ANTHROPIC_BASE_URL":"https://other.example"}}`
	writeRaw(t, path, external)
	if err := p.set(target, false); err == nil {
		t.Fatal("external change overwritten")
	}
	data, _ := os.ReadFile(path)
	if string(data) != external {
		t.Fatal("external change lost")
	}
	if p.view(target).Enabled {
		t.Fatal("external setting reported connected")
	}
}

// A slot listens on its own port, which the next deploy stops; Claude has to
// reach the router through the public address instead.
func TestClaudeProxyConnectsASlotThroughThePublicAddress(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROUTER_PROVIDERS_FILE", filepath.Join(dir, "providers.json"))
	t.Setenv("ROUTER_ENV_FILE", filepath.Join(dir, "env"))
	t.Setenv("ROUTER_LISTEN", "127.0.0.1:18791")
	t.Setenv("ROUTER_PUBLIC_LISTEN", "127.0.0.1:18787")
	u := newUIServer(newStore(10, ""), newConfigStore(loadConfig(), ""), newHealth(""))
	settings := filepath.Join(dir, "settings.json")
	u.claudeProxy = &claudeProxy{path: settings}
	r := httptest.NewRequest("POST", "http://localhost:8788/settings/claude-proxy", strings.NewReader(url.Values{"op": {"connect"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://localhost:8788")
	u.handler().ServeHTTP(httptest.NewRecorder(), r)
	data, err := os.ReadFile(settings)
	if err != nil || !strings.Contains(string(data), `"http://127.0.0.1:18787"`) {
		t.Fatalf("Claude pointed at the slot, not the public address: %v\n%s", err, data)
	}
}

func TestClaudeProxyButtonAndOrigin(t *testing.T) {
	cs := newConfigStore(config{listen: "127.0.0.1:8787"}, "")
	u := newUIServer(newStore(10, ""), cs, newHealth(""))
	u.claudeProxy = &claudeProxy{path: filepath.Join(t.TempDir(), "settings.json")}
	post := func(op, origin string) (int, string) {
		r := httptest.NewRequest("POST", "http://localhost:8788/settings/claude-proxy", strings.NewReader(url.Values{"op": {op}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		u.handler().ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	if code, _ := post("connect", "https://other.example"); code != 403 {
		t.Fatal("cross-origin mutation allowed")
	}
	if code, html := post("connect", "http://localhost:8788"); code != 200 || !strings.Contains(html, "Восстановить настройки Claude") {
		t.Fatalf("connect: %d %s", code, html)
	}
	if code, html := post("restore", "http://localhost:8788"); code != 200 || !strings.Contains(html, "Подключить Claude к роутеру") {
		t.Fatalf("restore: %d %s", code, html)
	}
}
