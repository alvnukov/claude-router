package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Both slots share limits.json during a deploy; only the active one writes it.
func TestRouterServerKeepsAnthropicLimitsInMemoryUntilActive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "limits.json")
	t.Setenv("ROUTER_ANTHROPIC_LIMITS_FILE", path)
	t.Setenv("ROUTER_PROVIDERS_FILE", filepath.Join(dir, "providers.json"))
	life := newLifecycle(true)
	server := newRouterServer(config{uiHistory: 10}, life, filepath.Join(dir, "state.json"))
	limits := server.ui.limits
	limits.observe(http.Header{"Anthropic-Ratelimit-Requests-Remaining": {"41"}}, time.Now())
	if err := limits.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("standby wrote limits.json: %v", err)
	}
	if got := limits.view(time.Now()).State; got != "fresh" {
		t.Fatalf("standby lost the observation: %s", got)
	}
	if err := life.activate(); err != nil {
		t.Fatal(err)
	}
	if err := limits.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("active slot did not write limits.json: %v", err)
	}
	limits.close()
}

// The legacy router is alone on history.jsonl; a slot is alone only once the
// deploy says the other slot has exited.
func TestRouterServerCompactsHistoryOnlyWhenNoOtherSlotAppends(t *testing.T) {
	for _, slot := range []string{"", "blue"} {
		t.Run("slot="+slot, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "history.jsonl")
			t.Setenv("ROUTER_UI_HISTORY_FILE", path)
			t.Setenv("ROUTER_PROVIDERS_FILE", filepath.Join(dir, "providers.json"))
			t.Setenv("ROUTER_ANTHROPIC_LIMITS_FILE", filepath.Join(dir, "limits.json"))
			t.Setenv("ROUTER_SLOT", slot)
			state := filepath.Join(dir, "state.json")
			server := newRouterServer(config{uiHistory: 2}, newLifecycle(false), state)
			lines := func() int {
				data, _ := os.ReadFile(path)
				return strings.Count(string(data), "\n")
			}
			for i := range 5 {
				server.st.persist(&record{ID: fmt.Sprintf("r-%d", i), End: time.Now()})
			}
			if slot == "" {
				if got := lines(); got != 2 {
					t.Fatalf("legacy router did not compact: %d lines", got)
				}
				return
			}
			if got := lines(); got != 5 {
				t.Fatalf("slot compacted before the deploy said it was alone: %d lines", got)
			}
			request := httptest.NewRequest(http.MethodPost, "/admin/compact", nil)
			request.RemoteAddr = "127.0.0.1:1"
			response := httptest.NewRecorder()
			server.runtimeAdmin(state).ServeHTTP(response, request)
			if response.Code != http.StatusNoContent || lines() != 2 {
				t.Fatalf("compact: HTTP %d, %d lines", response.Code, lines())
			}
			for i := range 3 {
				server.st.persist(&record{ID: fmt.Sprintf("s-%d", i), End: time.Now()})
			}
			if got := lines(); got != 2 {
				t.Fatalf("slot alone on the file did not keep compacting: %d lines", got)
			}
		})
	}
}

func TestRouterServerStandbyAndActivation(t *testing.T) {
	dir := t.TempDir()
	api, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ui, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "providers.json")
	if err := writeProviders(path, oneProvider("http://example.test/v1", "a", "b")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROUTER_PROVIDERS_FILE", path)
	cfg := config{listen: api.Addr().String(), uiListen: ui.Addr().String(), uiHistory: 10}
	life := newLifecycle(true)
	server := newRouterServer(cfg, life, filepath.Join(dir, "state.json"))
	done := make(chan error, 1)
	go func() { done <- server.serve(api, ui) }()
	client := &http.Client{Timeout: time.Second}
	get := func(addr, path string) (int, []byte) {
		t.Helper()
		resp, err := client.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode, nil
	}
	status, _ := get(cfg.listen, "/v1/messages")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("standby API status: %d", status)
	}
	resp, err := client.Get("http://" + cfg.listen + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var health struct {
		PID  int
		Mode string
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || health.PID != os.Getpid() || health.Mode != "standby" {
		t.Fatalf("standby health: %d %+v", resp.StatusCode, health)
	}
	request, _ := http.NewRequest(http.MethodPost, "http://"+cfg.listen+"/admin/activate", nil)
	resp, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("activate: %d", resp.StatusCode)
	}
	status, _ = get(cfg.uiListen, "/status")
	if status != http.StatusOK {
		t.Fatalf("active UI status: %d", status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("server did not exit")
	}
}
