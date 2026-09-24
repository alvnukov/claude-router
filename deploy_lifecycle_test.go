package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminDrainReportsPendingUntilStreamFinishes(t *testing.T) {
	life := newLifecycle(false)
	admin := newRuntimeAdmin(life, newHealth(""), "")
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan struct{})
	go func() {
		life.guard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			close(entered)
			<-release
		})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
		close(finished)
	}()
	<-entered
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/drain", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	admin.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("drain status: %d", w.Code)
	}
	w = httptest.NewRecorder()
	life.healthz(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var state struct {
		Pending int `json:"pending"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil || state.Pending != 1 {
		t.Fatalf("draining pending = %d, err %v", state.Pending, err)
	}
	close(release)
	<-finished
	w = httptest.NewRecorder()
	life.healthz(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil || state.Pending != 0 {
		t.Fatalf("finished pending = %d, err %v", state.Pending, err)
	}
}
