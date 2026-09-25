package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLifecycleTransitions(t *testing.T) {
	life := newLifecycle(true)
	if got := life.mode(); got != modeStandby {
		t.Fatalf("new standby mode = %s", got)
	}
	if life.acceptsTraffic() || life.writesSharedState() {
		t.Fatal("standby accepted traffic or wrote shared state")
	}
	if err := life.activate(); err != nil {
		t.Fatal(err)
	}
	if !life.acceptsTraffic() || !life.writesSharedState() {
		t.Fatal("activation did not enable traffic and writes")
	}
	if err := life.quiesce(); err != nil {
		t.Fatal(err)
	}
	if !life.acceptsTraffic() || life.writesSharedState() {
		t.Fatal("quiesced instance must serve requests without shared writes")
	}
	if err := life.activate(); err != nil {
		t.Fatalf("rollback before flip: %v", err)
	}
	if !life.writesSharedState() {
		t.Fatal("rollback did not restore shared writes")
	}
	life.drain()
	if life.acceptsTraffic() || life.writesSharedState() {
		t.Fatal("draining instance accepted traffic or wrote shared state")
	}
	if err := life.activate(); err == nil {
		t.Fatal("draining instance reactivated")
	}
}

func TestLifecycleHealthAndTraffic(t *testing.T) {
	life := newLifecycle(true)
	h := life.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		mode lifecycleMode
		want int
	}{
		{modeStandby, http.StatusServiceUnavailable},
		{modeActive, http.StatusNoContent},
		{modeQuiesced, http.StatusNoContent},
		{modeDraining, http.StatusServiceUnavailable},
	} {
		switch tc.mode {
		case modeActive:
			if err := life.activate(); err != nil {
				t.Fatal(err)
			}
		case modeQuiesced:
			if err := life.quiesce(); err != nil {
				t.Fatal(err)
			}
		case modeDraining:
			life.drain()
		}
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
		if r.Code != tc.want {
			t.Errorf("%s API status = %d, want %d", tc.mode, r.Code, tc.want)
		}
		r = httptest.NewRecorder()
		life.healthz(r, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		wantHealth := http.StatusOK
		if tc.mode == modeDraining {
			wantHealth = http.StatusServiceUnavailable
		}
		if r.Code != wantHealth {
			t.Errorf("%s health status = %d, want %d", tc.mode, r.Code, wantHealth)
		}
	}
}

func TestLifecycleDrainWaitsForOpenStreamAndTimesOut(t *testing.T) {
	life := newLifecycle(false)
	entered := make(chan struct{})
	finish := make(chan struct{})
	h := life.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-finish
		_, _ = w.Write([]byte("last event"))
	}))
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
		close(done)
	}()
	<-entered
	life.drain()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := life.wait(ctx); err != context.DeadlineExceeded {
		t.Fatalf("unfinished stream wait = %v, want timeout", err)
	}
	close(finish)
	<-done
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := life.wait(ctx); err != nil {
		t.Fatalf("finished stream wait: %v", err)
	}
}
