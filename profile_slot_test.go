package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestStandbyActivationReloadsSeparateProfilePointerWithoutWritingConfig(t *testing.T) {
	owner, h, path := profileFixture(t)
	if err := owner.ensureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := owner.createProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	standbySetup, err := readProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	standby := newConfigStore(config{local: standbySetup}, path)
	if err := owner.activateProfile("cloud", h); err != nil {
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
	if life.mode() != modeActive || standby.get().local.ActiveProfile != "cloud" {
		t.Fatalf("active mode=%s profile=%q", life.mode(), standby.get().local.ActiveProfile)
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
	legacy, err := readProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(`{"stats":{},"sessions":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	standby := newConfigStore(config{local: legacy}, path)
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
	saved, err := readProviders(path)
	if err != nil || saved.Pools != nil || saved.Routes == nil || standby.get().local.Routes == nil {
		t.Fatalf("migrated routes not saved and served: %v", err)
	}
}

// A Codex connection without auth_id gets one at start, but a standby slot
// only writes it once activation makes it the only writer.
func TestStandbySlotAssignsCodexIDsOnActivation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	raw := `{"providers":[{"name":"work","type":"codex","base_url":"` + codexBaseURL + `"}]}`
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
	standby := newConfigStore(c, path)
	r := &routerServer{cs: standby, health: newHealth(""), life: newLifecycle(true)}
	w := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/admin/activate", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	r.runtimeAdmin(statePath).ServeHTTP(w, request)
	if w.Code != http.StatusNoContent {
		t.Fatalf("standby activation: %d %s", w.Code, w.Body.String())
	}
	saved, err := readProviders(path)
	if err != nil {
		t.Fatalf("activation left providers.json without auth_id: %v", err)
	}
	onDisk, _ := saved.provider("work")
	served, _ := standby.get().local.provider("work")
	if !authIDOK(onDisk.AuthID) || served.AuthID != onDisk.AuthID {
		t.Fatalf("auth_id on disk %q, served %q", onDisk.AuthID, served.AuthID)
	}
	if backup, _ := os.ReadFile(path + ".before-codex-ids"); string(backup) != raw {
		t.Fatalf("backup missing or not the original: %q", backup)
	}
}
