package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemStateReportsSlotAndInstalledBinaryDigest(t *testing.T) {
	cfg := testDeployConfig(t)
	home := t.TempDir()
	binary := filepath.Join(home, "localrouter.blue")
	content := []byte("candidate binary")
	if err := os.WriteFile(binary, content, 0700); err != nil {
		t.Fatal(err)
	}
	cfg.BlueAPI = ""
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"slot": "blue", "pid": 101, "mode": "active", "pending": 3})
	}))
	defer backend.Close()
	cfg.BlueAPI = backend.Listener.Addr().String()
	ops := newSystemDeployOps(cfg, "http://127.0.0.1:1", home, filepath.Join(home, "agents"), binary)
	state, err := ops.state(context.Background(), "blue")
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(content)
	if state.PID != 101 || state.Pending != 3 || state.Digest != hex.EncodeToString(hash[:]) {
		t.Fatalf("state misses live PID or installed digest: %+v", state)
	}
}

func TestCaddyPersistentConfigIsValidAndTargetsSelectedSlot(t *testing.T) {
	skipDarwinOnlyDeploy(t)
	if _, err := exec.LookPath("caddy"); err != nil {
		t.Skip("caddy not installed")
	}
	cfg := testDeployConfig(t)
	home := t.TempDir()
	ops := newSystemDeployOps(cfg, "http://127.0.0.1:1", home, filepath.Join(home, "agents"), filepath.Join(home, "binary"))
	if err := ops.save(context.Background(), "green"); err != nil {
		t.Fatal(err)
	}
	if got, err := ops.adapt(context.Background(), filepath.Join(home, "Caddyfile")); err != nil || len(got) == 0 {
		t.Fatalf("Caddy could not adapt persisted config: %v", err)
	}
}

func TestCaddyAdaptedConfigListensOnLoopbackAndResolvesSlot(t *testing.T) {
	skipDarwinOnlyDeploy(t)
	if _, err := exec.LookPath("caddy"); err != nil {
		t.Skip("caddy not installed")
	}
	cfg := testDeployConfig(t)
	home := t.TempDir()
	ops := newSystemDeployOps(cfg, "http://127.0.0.1:1", home, filepath.Join(home, "agents"), filepath.Join(home, "binary"))
	if err := ops.save(context.Background(), "green"); err != nil {
		t.Fatal(err)
	}
	adapted, err := ops.adapt(context.Background(), filepath.Join(home, "Caddyfile"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Listen []string `json:"listen"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(adapted, &parsed); err != nil {
		t.Fatal(err)
	}
	for name, server := range parsed.Apps.HTTP.Servers {
		for _, listen := range server.Listen {
			if !strings.HasPrefix(listen, "127.0.0.1:") {
				t.Fatalf("Caddy server %s listens beyond loopback: %s", name, listen)
			}
		}
	}
	// The UI is opened as localhost as well as 127.0.0.1; a host matcher
	// would leave one of them without a route.
	if strings.Contains(string(adapted), `"host"`) {
		t.Fatalf("Caddy config matches on Host: %s", adapted)
	}
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(adapted) }))
	defer admin.Close()
	ops.adminURL = admin.URL
	if slot, err := ops.current(context.Background()); err != nil || slot != "green" {
		t.Fatalf("real Caddy config resolved to %q: %v\n%s", slot, err, adapted)
	}
}
