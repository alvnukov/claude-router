package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
)

type lifecycleMode string

const (
	modeStandby  lifecycleMode = "standby"
	modeActive   lifecycleMode = "active"
	modeQuiesced lifecycleMode = "quiesced"
	modeDraining lifecycleMode = "draining"
)

type lifecycle struct {
	mu       sync.Mutex
	state    lifecycleMode
	pending  int
	inflight sync.WaitGroup
	alone    bool // no other router appends to the shared history
}

// routerStartsStandby decides the start mode. A blue/green slot is active only
// when the active-slot marker names it, so launchd restarting the serving slot
// brings it back active while a freshly loaded candidate waits in standby.
func routerStartsStandby() bool {
	if os.Getenv("ROUTER_STANDBY") == "1" {
		return true
	}
	slot := os.Getenv("ROUTER_SLOT")
	marker := os.Getenv("ROUTER_ACTIVE_SLOT_FILE")
	if slot == "" || marker == "" {
		return false
	}
	data, err := os.ReadFile(marker)
	return err != nil || strings.TrimSpace(string(data)) != slot
}

func newLifecycle(standby bool) *lifecycle {
	state := modeActive
	if standby {
		state = modeStandby
	}
	return &lifecycle{state: state}
}

func (l *lifecycle) mode() lifecycleMode {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state
}

func (l *lifecycle) acceptsTraffic() bool {
	state := l.mode()
	return state == modeActive || state == modeQuiesced
}

func (l *lifecycle) writesSharedState() bool { return l.mode() == modeActive }

// markAlone records that no other router appends to the shared history, so
// this one may compact it. Leaving active mode forgets it: the next deploy
// starts another slot that appends too.
func (l *lifecycle) markAlone() {
	l.mu.Lock()
	l.alone = true
	l.mu.Unlock()
}

func (l *lifecycle) compactsHistory() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state == modeActive && l.alone
}

// CompactsHistory gates internal/history; a nil lifecycle is the only writer.
func (l *lifecycle) CompactsHistory() bool { return l == nil || l.compactsHistory() }

func (l *lifecycle) activate() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == modeDraining {
		return fmt.Errorf("cannot activate draining instance")
	}
	l.state = modeActive
	return nil
}

func (l *lifecycle) quiesce() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != modeActive && l.state != modeQuiesced {
		return fmt.Errorf("cannot quiesce %s instance", l.state)
	}
	l.state = modeQuiesced
	l.alone = false
	return nil
}

func (l *lifecycle) drain() {
	l.mu.Lock()
	l.state = modeDraining
	l.alone = false
	l.mu.Unlock()
}

func (l *lifecycle) wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { l.inflight.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *lifecycle) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		if l.state != modeActive && l.state != modeQuiesced {
			l.mu.Unlock()
			http.Error(w, "router not active", http.StatusServiceUnavailable)
			return
		}
		l.inflight.Add(1)
		l.pending++
		l.mu.Unlock()
		defer func() {
			l.mu.Lock()
			l.pending--
			l.mu.Unlock()
			l.inflight.Done()
		}()
		next.ServeHTTP(w, r)
	})
}

func (l *lifecycle) healthz(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	state, pending := l.state, l.pending
	l.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if state == modeDraining {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	// A failed write means the client is gone; there is no one to tell.
	_ = json.NewEncoder(w).Encode(struct {
		PID     int           `json:"pid"`
		Slot    string        `json:"slot"`
		Mode    lifecycleMode `json:"mode"`
		Pending int           `json:"pending"`
	}{os.Getpid(), os.Getenv("ROUTER_SLOT"), state, pending})
}
