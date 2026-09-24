package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRuntimeAdminTransitionsAndRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := newLifecycle(false)
	oldHealth := newHealth("")
	oldHealth.sessions = map[string]sessionBinding{"conversation": {Model: "p/m", Used: time.Now()}}
	oldAdmin := newRuntimeAdmin(old, oldHealth, path)
	post := func(handler http.Handler, endpoint string) int {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, endpoint, nil)
		r.RemoteAddr = "127.0.0.1:12345"
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if got := post(oldAdmin, "/admin/snapshot"); got != http.StatusNoContent {
		t.Fatalf("snapshot: %d", got)
	}
	if got := post(oldAdmin, "/admin/quiesce"); got != http.StatusNoContent {
		t.Fatalf("quiesce: %d", got)
	}
	if old.writesSharedState() {
		t.Fatal("old instance still writing after quiesce")
	}
	if got := post(oldAdmin, "/admin/activate"); got != http.StatusNoContent || !old.writesSharedState() {
		t.Fatalf("failed rollback before flip: %d", got)
	}
	post(oldAdmin, "/admin/quiesce")
	newHealth := newHealth("")
	newLife := newLifecycle(true)
	newAdmin := newRuntimeAdmin(newLife, newHealth, path)
	if got := post(newAdmin, "/admin/activate"); got != http.StatusNoContent {
		t.Fatalf("new activation: %d", got)
	}
	if newHealth.sessions["conversation"].Model != "p/m" {
		t.Fatalf("new slot missed handoff: %+v", newHealth.sessions)
	}
	if got := post(oldAdmin, "/admin/drain"); got != http.StatusNoContent || old.acceptsTraffic() {
		t.Fatalf("old drain: %d", got)
	}
	if got := post(oldAdmin, "/admin/activate"); got != http.StatusConflict {
		t.Fatalf("draining reactivation: %d", got)
	}
	if got := post(oldAdmin, "/admin/snapshot"); got != http.StatusConflict {
		t.Fatalf("draining snapshot: %d", got)
	}
	if got := post(oldAdmin, "/healthz"); got != http.StatusNotFound {
		t.Fatalf("admin wrong path: %d", got)
	}
	w := httptest.NewRecorder()
	oldAdmin.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/quiesce", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("admin GET: %d", w.Code)
	}
}

func TestRuntimeShutdownWaitsForStreamAndLeavesSlotSnapshot(t *testing.T) {
	dir := t.TempDir()
	h := newHealth("")
	h.record("p/m", true, time.Millisecond, "")
	life := newLifecycle(false)
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		life.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-release
		})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
		close(finished)
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := life.shutdown(ctx, h, filepath.Join(dir, "state.blue.json")); err != context.DeadlineExceeded {
		t.Fatalf("deadline not enforced: %v", err)
	}
	close(release)
	<-finished
	if err := life.shutdown(context.Background(), h, filepath.Join(dir, "state.blue.json")); err != nil {
		t.Fatalf("finished shutdown: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "state.blue.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot routerSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil || snapshot.Stats["p/m"].OK != 1 {
		t.Fatalf("slot snapshot: %v %+v", err, snapshot)
	}
}
