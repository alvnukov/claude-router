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
