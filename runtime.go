package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
)

type runtimeAdmin struct {
	life     *lifecycle
	health   *health
	state    string
	activate func() error
}

func newRuntimeAdmin(life *lifecycle, health *health, state string) *runtimeAdmin {
	return &runtimeAdmin{life: life, health: health, state: state}
}

func (a *runtimeAdmin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if addr, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && net.ParseIP(addr) != nil && !net.ParseIP(addr).IsLoopback() {
		http.Error(w, "local admin only", http.StatusForbidden)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		http.Error(w, "no browser admin requests", http.StatusForbidden)
		return
	}
	var err error
	switch r.URL.Path {
	case "/admin/snapshot":
		if a.life.mode() != modeActive {
			err = fmt.Errorf("cannot snapshot %s", a.life.mode())
		} else {
			err = a.health.saveSnapshot(a.state)
		}
	case "/admin/quiesce":
		err = a.life.quiesce()
	case "/admin/activate":
		if a.life.mode() == modeStandby {
			if err = a.health.loadSnapshot(a.state); err != nil {
				returnError(w, err)
				return
			}
		}
		if a.activate != nil {
			err = a.activate()
		}
		if err == nil {
			err = a.life.activate()
		}
	case "/admin/drain":
		a.life.drain()
	default:
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		returnError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func returnError(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusConflict)
}

func (l *lifecycle) shutdown(ctx context.Context, h *health, slotPath string) error {
	l.drain()
	err := l.wait(ctx)
	if slotPath != "" {
		if snapErr := h.saveSnapshot(slotPath); snapErr != nil {
			fmt.Fprintf(os.Stderr, "final snapshot: %v\n", snapErr)
		}
	}
	return err
}

func statePath() string {
	if p := os.Getenv("ROUTER_STATE_FILE"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "state.json")
	}
	return "state.json"
}
