package main

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	os.WriteFile(path, data, 0600)
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
				os.WriteFile(path, []byte(input), 0600)
			}
			if err := p.set(target, true); err != nil {
				t.Fatal(err)
			}
			if stat, err := os.Stat(path + ".router-proxy-backup"); err != nil || stat.Mode().Perm() != 0600 {
				t.Fatal("backup permissions")
			}
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
			json.Unmarshal(data, &got)
			json.Unmarshal([]byte(input), &want)
			a, _ := json.Marshal(got)
			b, _ := json.Marshal(want)
			if string(a) != string(b) {
				t.Fatalf("restored %s, want %s", a, b)
			}
		})
	}
	for _, input := range []string{`not JSON`, `null`, `{"env":[]}`} {
		path := filepath.Join(t.TempDir(), "settings.json")
		os.WriteFile(path, []byte(input), 0600)
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
	os.WriteFile(path, []byte(external), 0600)
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
