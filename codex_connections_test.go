package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testAuthA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testAuthB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func writeRaw(t *testing.T, path, raw string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexValidateRequiresAuthID(t *testing.T) {
	missing := localSetup{Providers: []provider{{Name: "work", Type: "codex"}}}
	if err := missing.Validate(); err == nil || !strings.Contains(err.Error(), "auth_id") {
		t.Fatalf("non-codex name without auth_id: %v", err)
	}
	legacy := localSetup{Providers: []provider{{Name: "codex", Type: "codex"}}}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy codex rejected: %v", err)
	}
	named := localSetup{Providers: []provider{{Name: "codex", Type: "codex", AuthID: testAuthA}, {Name: "work", Type: "codex", AuthID: testAuthB}}}
	if err := named.Validate(); err != nil {
		t.Fatalf("valid connections rejected: %v", err)
	}
	for name, bad := range map[string]localSetup{
		"malformed": {Providers: []provider{{Name: "work", Type: "codex", AuthID: "XYZ"}}},
		"non-codex": {Providers: []provider{{Name: "p", BaseURL: "http://x.test/v1", AuthID: testAuthA}}},
		"duplicate": {Providers: []provider{{Name: "a", Type: "codex", AuthID: testAuthA}, {Name: "b", Type: "codex", AuthID: testAuthA}}},
		"long name": {Providers: []provider{{Name: strings.Repeat("n", 65), Type: "codex", AuthID: testAuthA}}},
	} {
		if bad.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestCodexStartupAssignsIDsOnceWithBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	raw := `{"providers":[{"name":"codex","type":"codex","base_url":"` + CodexBaseURL + `"},{"name":"work","type":"codex","base_url":"` + CodexBaseURL + `"}]}`
	writeRaw(t, path, raw)
	t.Setenv("ROUTER_PROVIDERS_FILE", path)
	c, err := loadConfigChecked()
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := c.Local.Provider("codex")
	work, _ := c.Local.Provider("work")
	if legacy.AuthID != "" || !AuthIDOK(work.AuthID) {
		t.Fatalf("ids: %q %q", legacy.AuthID, work.AuthID)
	}
	if backup, _ := os.ReadFile(path + ".before-codex-ids"); string(backup) != raw {
		t.Fatalf("backup missing or not the original: %q", backup)
	}
	first, _ := os.ReadFile(path)
	if !strings.Contains(string(first), work.AuthID) {
		t.Fatal("assigned id not written")
	}
	again, err := loadConfigChecked()
	if err != nil {
		t.Fatal(err)
	}
	if w, _ := again.Local.Provider("work"); w.AuthID != work.AuthID {
		t.Fatalf("second start changed id: %q", w.AuthID)
	}
	if second, _ := os.ReadFile(path); string(second) != string(first) {
		t.Fatal("second start rewrote the file")
	}
	if _, err := ReadProviders(path); err != nil {
		t.Fatalf("migrated file is not strictly valid: %v", err)
	}
}

func TestCodexLegacyFileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := WriteProviders(path, localSetup{Providers: []provider{{Name: "codex", Type: "codex", BaseURL: CodexBaseURL}}}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if _, assigned, err := LoadLocal(path); err != nil || assigned {
		t.Fatalf("legacy load: assigned=%v err=%v", assigned, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || strings.Contains(string(after), "auth_id") {
		t.Fatal("legacy file rewritten")
	}
	if _, err := os.Stat(path + ".before-codex-ids"); !os.IsNotExist(err) {
		t.Fatal("legacy start made a backup")
	}
}

func TestCodexManualReloadNeverAssignsID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	raw := `{"providers":[{"name":"work","type":"codex","base_url":"` + CodexBaseURL + `"}]}`
	writeRaw(t, path, raw)
	if _, err := ReadProviders(path); err == nil || !strings.Contains(err.Error(), "auth_id") {
		t.Fatalf("strict read accepted: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != raw {
		t.Fatal("strict read rewrote the file")
	}
}

func TestCodexProviderAddAssignsFreshIDAndRejectsRename(t *testing.T) {
	u, h := testUI(t)
	u.cs = NewStore(u.cs.Get(), filepath.Join(t.TempDir(), "providers.json"))
	get(t, h, "POST", "/settings/providers", url.Values{"op": {"add"}, "name": {"work"}, "type": {"codex"}})
	p, ok := u.cs.Get().Local.Provider("work")
	if !ok || !AuthIDOK(p.AuthID) {
		t.Fatalf("add gave id %q", p.AuthID)
	}
	w := get(t, h, "POST", "/settings/providers", url.Values{"op": {"update"}, "orig": {"work"}, "name": {"work2"}})
	if !strings.Contains(w.Body.String(), "переименовать") {
		t.Fatal("codex rename not rejected")
	}
	if _, ok := u.cs.Get().Local.Provider("work"); !ok {
		t.Fatal("rejected rename changed the config")
	}
	get(t, h, "POST", "/settings/providers", url.Values{"op": {"remove"}, "name": {"work"}})
	get(t, h, "POST", "/settings/providers", url.Values{"op": {"add"}, "name": {"work"}, "type": {"codex"}})
	again, _ := u.cs.Get().Local.Provider("work")
	if !AuthIDOK(again.AuthID) || again.AuthID == p.AuthID {
		t.Fatal("recreated provider reused the old id")
	}
}

func useTestCodexHome(t *testing.T, issuer string, client *http.Client) {
	t.Helper()
	old := codexAuth
	t.Cleanup(func() { codexAuth = old })
	codexAuth = &codexAuthStore{path: filepath.Join(t.TempDir(), "codex-auth.json"), cliPath: filepath.Join(t.TempDir(), "cli.json"), issuer: issuer, client: client}
}

func TestCodexStoreForSeparatesConnections(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	legacy, err := codexStoreFor(provider{Name: "codex", Type: "codex"})
	if err != nil || legacy != codexAuth {
		t.Fatalf("legacy store: %v", err)
	}
	a, _ := codexStoreFor(provider{Name: "codex", Type: "codex", AuthID: testAuthA})
	b, _ := codexStoreFor(provider{Name: "work", Type: "codex", AuthID: testAuthB})
	if a == codexAuth || a == b || a.path == b.path || filepath.Dir(a.path) != filepath.Dir(codexAuth.path) {
		t.Fatalf("paths %q %q", a.path, b.path)
	}
	if again, _ := codexStoreFor(provider{Name: "work", Type: "codex", AuthID: testAuthB}); again != b {
		t.Fatal("store not cached")
	}
	if _, err := codexStoreFor(provider{Name: "work", Type: "codex"}); err == nil {
		t.Fatal("non-legacy without id got a store")
	}
	if err := codexAuth.save(usageCredential("legacy")); err != nil {
		t.Fatal(err)
	}
	if b.connected() {
		t.Fatal("new connection adopted the legacy token")
	}
	if err := b.save(usageCredential("old-work")); err != nil {
		t.Fatal(err)
	}
	recreated, _ := codexStoreFor(provider{Name: "work", Type: "codex", AuthID: strings.Repeat("c", 32)})
	if recreated.connected() {
		t.Fatal("recreated name adopted the old id's token")
	}
}

func codexUI(t *testing.T) (*uiServer, http.Handler) {
	t.Helper()
	u, h := testUI(t)
	l := u.cs.Get().Local.Clone()
	l.Providers = append(l.Providers,
		provider{Name: "codex", Type: "codex", BaseURL: CodexBaseURL},
		provider{Name: "work", Type: "codex", BaseURL: CodexBaseURL, AuthID: testAuthB})
	c := u.cs.Get()
	c.Local = l
	u.cs = NewStore(c, "")
	return u, h
}

func TestCodexActionsRejectUnknownProvider(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	_, h := codexUI(t)
	for _, target := range []string{"/settings/codex/import", "/settings/codex/login", "/settings/codex/usage"} {
		for _, name := range []string{"", "missing", "p"} {
			req := httptest.NewRequest("POST", target, strings.NewReader(url.Values{"provider": {name}}.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s provider=%q: %d", target, name, w.Code)
			}
		}
	}
	if entries, _ := os.ReadDir(filepath.Dir(codexAuth.path)); len(entries) != 0 {
		t.Fatalf("files written: %v", entries)
	}
}

func TestCodexImportTargetsNamedProvider(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	u, h := codexUI(t)
	data, _ := json.Marshal(usageCredential("work-account"))
	writeRaw(t, codexAuth.cliPath, string(data))
	get(t, h, "POST", "/settings/codex/import", url.Values{"provider": {"work"}})
	work, _ := codexStoreFor(provider{Name: "work", Type: "codex", AuthID: testAuthB})
	if !work.connected() || codexAuth.connected() {
		t.Fatal("import went to the wrong connection")
	}
	if v := u.codexLoginViewFor(provider{Name: "codex", Type: "codex"}); v.Connected {
		t.Fatal("legacy shows work's login")
	}
}

func seedConnection(t *testing.T, p provider, account string) codexCredential {
	t.Helper()
	s, err := codexStoreFor(p)
	if err != nil {
		t.Fatal(err)
	}
	c := usageCredential(account)
	// A distinct expiry per account gives each connection its own valid token.
	c.Tokens.AccessToken = testJWT(time.Now().Add(time.Hour + time.Duration(account[len(account)-1])*time.Minute))
	if err := s.save(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCodexRequestsUseTheirOwnAccount(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	a := provider{Name: "codex", Type: "codex", BaseURL: CodexBaseURL}
	b := provider{Name: "work", Type: "codex", BaseURL: CodexBaseURL, AuthID: testAuthB}
	ca, cb := seedConnection(t, a, "acct-a"), seedConnection(t, b, "acct-b")
	seen := map[string]string{}
	var mu sync.Mutex
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		seen[r.Header.Get("ChatGPT-Account-Id")] = r.Header.Get("Authorization")
		mu.Unlock()
		return usageResponse(200, `{"ok":true}`), nil
	})
	for _, p := range []provider{a, b} {
		res := tryModel(httptest.NewRequest("POST", "/", nil), config{FirstByte: time.Second}, candidate{Key: p.Name + "/m", Provider: p}, []byte(`{}`), false)
		if res.err != nil {
			t.Fatal(res.err)
		}
		res.resp.Body.Close()
		res.cancel()
	}
	if seen["acct-a"] != "Bearer "+ca.Tokens.AccessToken || seen["acct-b"] != "Bearer "+cb.Tokens.AccessToken {
		t.Fatalf("accounts mixed: %v", seen)
	}
}

func TestCodexProbeUsesOwnAccount(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	u, _ := codexUI(t)
	a, _ := u.cs.Get().Local.Provider("codex")
	b, _ := u.cs.Get().Local.Provider("work")
	seedConnection(t, a, "acct-a")
	seedConnection(t, b, "acct-b")
	var seen []string
	var mu sync.Mutex
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		seen = append(seen, r.Header.Get("ChatGPT-Account-Id"))
		mu.Unlock()
		return usageResponse(200, `{"models":[]}`), nil
	})
	u.probeProvider(a, true)
	u.probeProvider(b, true)
	if strings.Join(seen, ",") != "acct-a,acct-b" {
		t.Fatalf("probes: %v", seen)
	}
	// "work" removed and added again: same name, new id, new login.
	recreated := b
	recreated.AuthID = strings.Repeat("c", 32)
	seedConnection(t, recreated, "acct-c")
	u.probeProvider(recreated, false)
	if strings.Join(seen, ",") != "acct-a,acct-b,acct-c" {
		t.Fatalf("probe cache ignored the new id: %v", seen)
	}
}
