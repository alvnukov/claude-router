package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"localrouter/internal/history"
)

// A standby slot leaves Codex alone; the slot that becomes active starts the
// background usage refresh, so the limits keep updating after a switch.
func TestCodexUsageRefreshStartsWhenASlotActivates(t *testing.T) {
	oldAuth := codexAuth
	codexAuth = &codexAuthStore{loaded: true, credential: usageCredential("acct")}
	defer func() { codexAuth = oldAuth }()
	dir := t.TempDir()
	providers := filepath.Join(dir, "providers.json")
	if err := os.WriteFile(providers, []byte(`{"providers":[{"name":"codex","type":"codex","base_url":"`+CodexBaseURL+`"}],"models":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	local, err := ReadProviders(providers)
	if err != nil {
		t.Fatal(err)
	}
	life := newLifecycle(true)
	u := newUIServer(history.New(10, ""), NewStore(config{Local: local}, providers), newHealth(""))
	u.life = life
	u.fetchAnthropic = func(context.Context) ([]string, error) { return nil, nil }
	calls := make(chan struct{}, 10)
	u.codexUsage.client = &http.Client{Transport: usageTransport(func(*http.Request) (*http.Response, error) {
		calls <- struct{}{}
		return usageResponse(200, `{"rate_limit":{"primary_window":{"used_percent":25}}}`), nil
	})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := &routerServer{cs: u.cs, health: newHealth(""), life: life, ui: u, background: ctx, cancel: cancel}
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(statePath, []byte(`{"stats":{},"sessions":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/admin/activate", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	r.runtimeAdmin(statePath).ServeHTTP(w, request)
	if w.Code != http.StatusNoContent {
		t.Fatalf("activation: %d %s", w.Code, w.Body.String())
	}
	select {
	case <-calls:
	case <-time.After(5 * time.Second):
		t.Fatal("the activated slot did not refresh Codex usage")
	}
	// The catalog refresh started beside it writes providers.json; let it
	// finish before the directory goes away.
	for deadline := time.Now().Add(5 * time.Second); u.cs.Get().Local.Catalog.CheckedAt.IsZero(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("catalog refresh did not finish")
		}
	}
}

// Ticks reach the refresh only while the router is the active writer: a
// standby, quiesced or draining slot sends nothing to Codex.
func TestCodexUsageTicksOnlyWhileActive(t *testing.T) {
	life := newLifecycle(true)
	in := make(chan time.Time)
	out := activeTicks(t.Context(), life, in)
	tick := func() bool {
		t.Helper()
		in <- time.Now()
		select {
		case <-out:
			return true
		case <-time.After(100 * time.Millisecond):
			return false
		}
	}
	if tick() {
		t.Fatal("standby slot ticked")
	}
	if err := life.activate(); err != nil {
		t.Fatal(err)
	}
	if !tick() {
		t.Fatal("active slot did not tick")
	}
	if err := life.quiesce(); err != nil {
		t.Fatal(err)
	}
	if tick() {
		t.Fatal("quiesced slot ticked")
	}
	life.drain()
	if tick() {
		t.Fatal("draining slot ticked")
	}
}
