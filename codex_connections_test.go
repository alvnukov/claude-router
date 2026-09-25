package main

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if err := missing.validate(); err == nil || !strings.Contains(err.Error(), "auth_id") {
		t.Fatalf("non-codex name without auth_id: %v", err)
	}
	legacy := localSetup{Providers: []provider{{Name: "codex", Type: "codex"}}}
	if err := legacy.validate(); err != nil {
		t.Fatalf("legacy codex rejected: %v", err)
	}
	named := localSetup{Providers: []provider{{Name: "codex", Type: "codex", AuthID: testAuthA}, {Name: "work", Type: "codex", AuthID: testAuthB}}}
	if err := named.validate(); err != nil {
		t.Fatalf("valid connections rejected: %v", err)
	}
	for name, bad := range map[string]localSetup{
		"malformed": {Providers: []provider{{Name: "work", Type: "codex", AuthID: "XYZ"}}},
		"non-codex": {Providers: []provider{{Name: "p", BaseURL: "http://x.test/v1", AuthID: testAuthA}}},
		"duplicate": {Providers: []provider{{Name: "a", Type: "codex", AuthID: testAuthA}, {Name: "b", Type: "codex", AuthID: testAuthA}}},
		"long name": {Providers: []provider{{Name: strings.Repeat("n", 65), Type: "codex", AuthID: testAuthA}}},
	} {
		if bad.validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestCodexStartupAssignsIDsOnceWithBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	raw := `{"providers":[{"name":"codex","type":"codex","base_url":"` + codexBaseURL + `"},{"name":"work","type":"codex","base_url":"` + codexBaseURL + `"}]}`
	writeRaw(t, path, raw)
	t.Setenv("ROUTER_PROVIDERS_FILE", path)
	c, err := loadConfigChecked()
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := c.local.provider("codex")
	work, _ := c.local.provider("work")
	if legacy.AuthID != "" || !authIDOK(work.AuthID) {
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
	if w, _ := again.local.provider("work"); w.AuthID != work.AuthID {
		t.Fatalf("second start changed id: %q", w.AuthID)
	}
	if second, _ := os.ReadFile(path); string(second) != string(first) {
		t.Fatal("second start rewrote the file")
	}
	if _, err := readProviders(path); err != nil {
		t.Fatalf("migrated file is not strictly valid: %v", err)
	}
}

func TestCodexLegacyFileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := writeProviders(path, localSetup{Providers: []provider{{Name: "codex", Type: "codex", BaseURL: codexBaseURL}}}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if _, assigned, err := loadLocalSetupChecked(path); err != nil || assigned {
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
	raw := `{"providers":[{"name":"work","type":"codex","base_url":"` + codexBaseURL + `"}]}`
	writeRaw(t, path, raw)
	if _, err := readProviders(path); err == nil || !strings.Contains(err.Error(), "auth_id") {
		t.Fatalf("strict read accepted: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != raw {
		t.Fatal("strict read rewrote the file")
	}
}

func TestCodexProviderAddAssignsFreshIDAndRejectsRename(t *testing.T) {
	u, h := testUI(t)
	u.cs.provPath = filepath.Join(t.TempDir(), "providers.json")
	get(t, h, "POST", "/settings/providers", url.Values{"op": {"add"}, "name": {"work"}, "type": {"codex"}})
	p, ok := u.cs.get().local.provider("work")
	if !ok || !authIDOK(p.AuthID) {
		t.Fatalf("add gave id %q", p.AuthID)
	}
	w := get(t, h, "POST", "/settings/providers", url.Values{"op": {"update"}, "orig": {"work"}, "name": {"work2"}})
	if !strings.Contains(w.Body.String(), "переименовать") {
		t.Fatal("codex rename not rejected")
	}
	if _, ok := u.cs.get().local.provider("work"); !ok {
		t.Fatal("rejected rename changed the config")
	}
	get(t, h, "POST", "/settings/providers", url.Values{"op": {"remove"}, "name": {"work"}})
	get(t, h, "POST", "/settings/providers", url.Values{"op": {"add"}, "name": {"work"}, "type": {"codex"}})
	again, _ := u.cs.get().local.provider("work")
	if !authIDOK(again.AuthID) || again.AuthID == p.AuthID {
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
