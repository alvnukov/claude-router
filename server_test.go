package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
	json.NewDecoder(resp.Body).Decode(&health)
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
