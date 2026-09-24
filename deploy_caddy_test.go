package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// startCaddy runs a real Caddy on the test's own ports and returns its stop.
// Its data and config directories are temporary, so an autosave never
// reaches the user's Caddy.
func startCaddy(t *testing.T, home, caddyfile string) func() {
	t.Helper()
	cmd := exec.Command("caddy", "run", "--config", caddyfile, "--adapter", "caddyfile")
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_DATA_HOME="+home, "XDG_CONFIG_HOME="+home)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() { once.Do(func() { cmd.Process.Kill(); cmd.Wait() }) }
	t.Cleanup(stop)
	return stop
}

func waitCaddy(t *testing.T, ops *systemDeployOps, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		slot, err := ops.current(t.Context())
		if err == nil && slot == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Caddy serves %q, want %q: %v", slot, want, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func publicSlot(t *testing.T, address string) string {
	t.Helper()
	resp, err := http.Get("http://" + address + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func TestRealCaddyFlipRoutesToNewSlotAndSurvivesRestart(t *testing.T) {
	if _, err := exec.LookPath("caddy"); err != nil {
		t.Skip("caddy not installed")
	}
	cfg := testDeployConfig(t)
	backend := func(slot string) string {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, slot) }))
		t.Cleanup(s.Close)
		return s.Listener.Addr().String()
	}
	cfg.BlueAPI, cfg.GreenAPI = backend("blue"), backend("green")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	admin := listener.Addr().String()
	listener.Close()
	home := t.TempDir()
	ops := newSystemDeployOps(cfg, "http://"+admin, home, filepath.Join(home, "agents"), filepath.Join(home, "binary"))
	if err := ops.save(t.Context(), "blue"); err != nil {
		t.Fatal(err)
	}
	caddyfile := filepath.Join(home, "Caddyfile")

	stop := startCaddy(t, home, caddyfile)
	waitCaddy(t, ops, "blue")
	if got := publicSlot(t, cfg.PublicAPI); got != "blue" {
		t.Fatalf("public API reached %q before the flip", got)
	}
	if err := ops.flip(t.Context(), "green"); err != nil {
		t.Fatal(err)
	}
	waitCaddy(t, ops, "green")
	if got := publicSlot(t, cfg.PublicAPI); got != "green" {
		t.Fatalf("public API reached %q after the flip", got)
	}
	if marker, err := os.ReadFile(filepath.Join(home, "active-slot")); err != nil || strings.TrimSpace(string(marker)) != "green" {
		t.Fatalf("marker after flip: %q %v", marker, err)
	}

	stop()
	startCaddy(t, home, caddyfile)
	waitCaddy(t, ops, "green")
	if got := publicSlot(t, cfg.PublicAPI); got != "green" {
		t.Fatalf("restarted Caddy went back to %q", got)
	}
}
