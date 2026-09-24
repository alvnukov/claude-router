package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaddyAdminReadsBothUpstreamsAndRejectsDisagreement(t *testing.T) {
	cfg := testDeployConfig(t)
	body := func(api, ui string) []byte {
		return []byte(`{"apps":{"http":{"servers":{"api":{"listen":["`+cfg.PublicAPI+`"],"routes":[{"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"`+api+`"}]}]}]},"ui":{"listen":["`+cfg.PublicUI+`"],"routes":[{"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"`+ui+`"}]}]}]}}}}}`)
	}
	config := body(cfg.BlueAPI, cfg.BlueUI)
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(config) }))
	defer admin.Close()
	ops := newSystemDeployOps(cfg, admin.URL, t.TempDir(), filepath.Join(t.TempDir(), "agents"), filepath.Join(t.TempDir(), "binary"))
	slot, err := ops.current(t.Context())
	if err != nil || slot != "blue" { t.Fatalf("resolved slot = %s: %v", slot, err) }
	config = body(cfg.BlueAPI, cfg.GreenUI)
	if _, err := ops.current(t.Context()); err == nil { t.Fatal("accepted split API/UI upstreams") }
}

func TestCaddyConfigKeepsBothLoopbackPortsAndStreams(t *testing.T) {
	cfg := testDeployConfig(t)
	ops := newSystemDeployOps(cfg, "http://127.0.0.1:1", t.TempDir(), filepath.Join(t.TempDir(), "agents"), filepath.Join(t.TempDir(), "binary"))
	text := ops.caddyfile("green")
	for _, expected := range []string{cfg.PublicAPI, cfg.PublicUI, cfg.GreenAPI, cfg.GreenUI, "flush_interval -1", "stream_close_delay 16m"} {
		if !strings.Contains(text, expected) { t.Fatalf("Caddy config missing %s", expected) }
	}
	if strings.Contains(text, cfg.BlueAPI) || strings.Contains(text, cfg.BlueUI) { t.Fatal("Caddy config retained old upstream") }
}

func TestLaunchdSlotConfigSetsExitTimeoutAndSharedPaths(t *testing.T) {
	cfg := testDeployConfig(t)
	home := t.TempDir()
	ops := newSystemDeployOps(cfg, "http://127.0.0.1:1", home, filepath.Join(home, "agents"), filepath.Join(home, "binary"))
	p := ops.slotPlist("green")
	if p.ExitTimeOut < 960 || p.EnvironmentVariables["ROUTER_LISTEN"] != cfg.GreenAPI || p.EnvironmentVariables["ROUTER_UI_LISTEN"] != cfg.GreenUI || p.EnvironmentVariables["ROUTER_STANDBY"] != "1" {
		t.Fatalf("unsafe launchd slot: %+v", p)
	}
	if p.EnvironmentVariables["ROUTER_PROVIDERS_FILE"] != filepath.Join(home, "providers.json") { t.Fatal("slot lost shared config") }
}

func TestRealAdminEndpointsFollowLoopbackOnly(t *testing.T) {
	life := newLifecycle(false)
	backend := httptest.NewServer(newRuntimeAdmin(life, newHealth(""), filepath.Join(t.TempDir(), "snapshot.json")))
	defer backend.Close()
	resp, err := http.Post(backend.URL+"/admin/quiesce", "", nil)
	if err != nil { t.Fatal(err) }
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || life.mode() != modeQuiesced { t.Fatalf("admin quiesce: %d %s", resp.StatusCode, life.mode()) }
}

func testDeployConfig(t *testing.T) deployConfig {
	t.Helper()
	address := func() string {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil { t.Fatal(err) }
		defer listener.Close()
		return listener.Addr().String()
	}
	c := deployConfig{PublicAPI: address(), PublicUI: address(), BlueAPI: address(), BlueUI: address(), GreenAPI: address(), GreenUI: address()}
	if err := c.validate(true); err != nil { t.Fatal(err) }
	return c
}

var _, _ = context.Background, json.Unmarshal
var _ = os.ErrNotExist
