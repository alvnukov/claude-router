package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	conf "localrouter/internal/config"
)

func TestStandbyActivationReloadsSeparateProfilePointerWithoutWritingConfig(t *testing.T) {
	owner, _, path := profileFixture(t)
	if err := owner.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := owner.CreateProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	standbySetup, err := conf.ReadProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	standby := conf.NewStore(config{Local: standbySetup}, path)
	if err := owner.ActivateProfile("cloud"); err != nil {
		t.Fatal(err)
	}
	providerBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pointerBefore, err := os.ReadFile(path + ".active-profile")
	if err != nil {
		t.Fatal(err)
	}
	backupBefore, err := os.ReadFile(path + ".before-profiles")
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(`{"stats":{},"sessions":{},"active_profile":"default"}`), 0600); err != nil {
		t.Fatal(err)
	}
	life := newLifecycle(true)
	r := &routerServer{cs: standby, health: newHealth(""), life: life}
	w := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/admin/activate", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	r.runtimeAdmin(statePath).ServeHTTP(w, request)
	if w.Code != http.StatusNoContent {
		t.Fatalf("standby activation: %d %s", w.Code, w.Body.String())
	}
	if life.mode() != modeActive || standby.Get().Local.ActiveProfile != "cloud" {
		t.Fatalf("active mode=%s profile=%q", life.mode(), standby.Get().Local.ActiveProfile)
	}
	providerAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pointerAfter, err := os.ReadFile(path + ".active-profile")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(providerBefore, providerAfter) || !bytes.Equal(pointerBefore, pointerAfter) {
		t.Fatal("slot activation wrote providers or profile pointer")
	}
	backupAfter, err := os.ReadFile(path + ".before-profiles")
	if err != nil || !bytes.Equal(backupBefore, backupAfter) {
		t.Fatalf("slot changed migration backup: %v", err)
	}
}

// A standby slot skips the config migrations at start; they run once it is
// the only writer.
func TestStandbyActivationRunsConfigMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, []byte(`{"providers":[{"name":"p","base_url":"http://h/v1"}],"models":[{"provider":"p","model":"a"}],"pools":{"opus":["p/a"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	legacy, err := conf.ReadProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(`{"stats":{},"sessions":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	standby := conf.NewStore(config{Local: legacy}, path)
	r := &routerServer{cs: standby, health: newHealth(""), life: newLifecycle(true)}
	w := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/admin/activate", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	r.runtimeAdmin(statePath).ServeHTTP(w, request)
	if w.Code != http.StatusNoContent {
		t.Fatalf("standby activation: %d %s", w.Code, w.Body.String())
	}
	for _, suffix := range []string{".before-pools", ".active-profile"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Fatalf("activation skipped a migration: %v", err)
		}
	}
	saved, err := conf.ReadProviders(path)
	if err != nil || saved.Pools != nil || saved.Routes == nil || standby.Get().Local.Routes == nil {
		t.Fatalf("migrated routes not saved and served: %v", err)
	}
}

// A Codex connection without auth_id gets one at start, but a standby slot
// only writes it once activation makes it the only writer.
func TestStandbySlotAssignsCodexIDsOnActivation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	raw := `{"providers":[{"name":"work","type":"codex","base_url":"` + conf.CodexBaseURL + `"}]}`
	writeRaw(t, path, raw)
	t.Setenv("ROUTER_PROVIDERS_FILE", path)
	t.Setenv("ROUTER_STANDBY", "1")
	c, err := loadConfigChecked()
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != raw {
		t.Fatal("standby start rewrote providers.json")
	}
	if _, err := os.Stat(path + ".before-codex-ids"); !os.IsNotExist(err) {
		t.Fatal("standby start made a backup")
	}
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(`{"stats":{},"sessions":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	standby := conf.NewStore(c, path)
	r := &routerServer{cs: standby, health: newHealth(""), life: newLifecycle(true)}
	w := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/admin/activate", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	r.runtimeAdmin(statePath).ServeHTTP(w, request)
	if w.Code != http.StatusNoContent {
		t.Fatalf("standby activation: %d %s", w.Code, w.Body.String())
	}
	saved, err := conf.ReadProviders(path)
	if err != nil {
		t.Fatalf("activation left providers.json without auth_id: %v", err)
	}
	onDisk, _ := saved.Provider("work")
	served, _ := standby.Get().Local.Provider("work")
	if !conf.AuthIDOK(onDisk.AuthID) || served.AuthID != onDisk.AuthID {
		t.Fatalf("auth_id on disk %q, served %q", onDisk.AuthID, served.AuthID)
	}
	if backup, _ := os.ReadFile(path + ".before-codex-ids"); string(backup) != raw {
		t.Fatalf("backup missing or not the original: %q", backup)
	}
}
