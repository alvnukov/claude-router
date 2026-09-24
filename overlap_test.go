package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQuiescedHealthKeepsMemoryWithoutWritingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	life := newLifecycle(false)
	h := newHealth(path)
	h.life = life
	h.record("provider/model", true, time.Millisecond, "")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := life.quiesce(); err != nil {
		t.Fatal(err)
	}
	h.record("provider/model", false, 0, "failure")
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatalf("quiesced health wrote shared file: %v", err)
	}
	if h.snapshot("provider/model").Fail != 1 {
		t.Fatal("quiesced health lost in-memory outcome")
	}
}

func TestQuiescedUIRejectsChangesButServesReads(t *testing.T) {
	life := newLifecycle(false)
	u := newUIServer(newStore(5, ""), newConfigStore(config{}, ""), newHealth(""))
	u.life = life
	if err := life.quiesce(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://localhost/settings/refresh-models", nil)
	w := httptest.NewRecorder()
	u.handler().ServeHTTP(w, request)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("quiesced UI mutation = %d", w.Code)
	}
	w = httptest.NewRecorder()
	u.handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://localhost/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("quiesced UI read = %d", w.Code)
	}
}

func TestStandbyCatalogDoesNotTouchProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	life := newLifecycle(true)
	cs := newConfigStore(config{}, path)
	u := newUIServer(newStore(5, ""), cs, newHealth(""))
	u.life = life
	if err := u.refreshModels(t.Context()); err == nil {
		t.Fatal("standby catalog refresh should reject instead of writing")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "original" {
		t.Fatal("standby catalog changed providers")
	}
}
