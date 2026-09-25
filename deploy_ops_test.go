package main

import (
	"context"
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
		return []byte(`{"apps":{"http":{"servers":{"api":{"listen":["` + cfg.PublicAPI + `"],"routes":[{"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"` + api + `"}]}]}]},"ui":{"listen":["` + cfg.PublicUI + `"],"routes":[{"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"` + ui + `"}]}]}]}}}}}`)
	}
	config := body(cfg.BlueAPI, cfg.BlueUI)
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(config) }))
	defer admin.Close()
	ops := newSystemDeployOps(cfg, admin.URL, t.TempDir(), filepath.Join(t.TempDir(), "agents"), filepath.Join(t.TempDir(), "binary"))
	slot, err := ops.current(t.Context())
	if err != nil || slot != "blue" {
		t.Fatalf("resolved slot = %s: %v", slot, err)
	}
	config = body(cfg.BlueAPI, cfg.GreenUI)
	if _, err := ops.current(t.Context()); err == nil {
		t.Fatal("accepted split API/UI upstreams")
	}
}

func TestCaddyConfigKeepsBothLoopbackPortsAndStreams(t *testing.T) {
	cfg := testDeployConfig(t)
	ops := newSystemDeployOps(cfg, "http://127.0.0.1:1", t.TempDir(), filepath.Join(t.TempDir(), "agents"), filepath.Join(t.TempDir(), "binary"))
	text := ops.caddyfile("green")
	_, apiPort, _ := net.SplitHostPort(cfg.PublicAPI)
	_, uiPort, _ := net.SplitHostPort(cfg.PublicUI)
	for _, expected := range []string{"http://:" + apiPort + " ", "http://:" + uiPort + " ", "bind 127.0.0.1", cfg.GreenAPI, cfg.GreenUI, "flush_interval -1", "stream_close_delay 16m", "persist_config off"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("Caddy config missing %s", expected)
		}
	}
	if strings.Contains(text, cfg.BlueAPI) || strings.Contains(text, cfg.BlueUI) {
		t.Fatal("Caddy config retained old upstream")
	}
}

func TestLaunchdSlotConfigSetsExitTimeoutAndSharedPaths(t *testing.T) {
	cfg := testDeployConfig(t)
	home := t.TempDir()
	ops := newSystemDeployOps(cfg, "http://127.0.0.1:1", home, filepath.Join(home, "agents"), filepath.Join(home, "binary"))
	p := ops.slotPlist("green")
	if p.ExitTimeOut < 960 || p.EnvironmentVariables["ROUTER_LISTEN"] != cfg.GreenAPI || p.EnvironmentVariables["ROUTER_UI_LISTEN"] != cfg.GreenUI || p.EnvironmentVariables["ROUTER_ACTIVE_SLOT_FILE"] != filepath.Join(home, "active-slot") || p.EnvironmentVariables["ROUTER_STANDBY"] != "" {
		t.Fatalf("unsafe launchd slot: %+v", p)
	}
	if p.EnvironmentVariables["ROUTER_PROVIDERS_FILE"] != filepath.Join(home, "providers.json") {
		t.Fatal("slot lost shared config")
	}
	if p.EnvironmentVariables["ROUTER_ANTHROPIC_LIMITS_FILE"] != filepath.Join(home, "limits.json") {
		t.Fatal("slot lost shared Anthropic limits")
	}
	if p.EnvironmentVariables["ROUTER_PUBLIC_LISTEN"] != cfg.PublicAPI {
		t.Fatal("slot does not know the public address clients use")
	}
}

func TestRealAdminEndpointsFollowLoopbackOnly(t *testing.T) {
	life := newLifecycle(false)
	backend := httptest.NewServer(newRuntimeAdmin(life, newHealth(""), filepath.Join(t.TempDir(), "snapshot.json")))
	defer backend.Close()
	resp, err := http.Post(backend.URL+"/admin/quiesce", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || life.mode() != modeQuiesced {
		t.Fatalf("admin quiesce: %d %s", resp.StatusCode, life.mode())
	}
}

func TestDeployOpsUseConfiguredScratchLabels(t *testing.T) {
	cfg := testDeployConfig(t)
	home := t.TempDir()
	file := deployFile{CaddyAdmin: "127.0.0.1:1", LabelPrefix: "com.claude-local-router.scratch-123", deployConfig: cfg}
	ops := newDeployOps(file, home, filepath.Join(home, "agents"), filepath.Join(home, "binary")).(*systemDeployOps)
	if got := ops.slotPlist("blue").Label; got != file.LabelPrefix+".blue" {
		t.Fatalf("blue label %q", got)
	}
	var calls []string
	ops.launchctl = func(_ context.Context, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	if err := ops.stop(t.Context(), "green"); err != nil || len(calls) != 1 || !strings.Contains(calls[0], file.LabelPrefix+".green") {
		t.Fatalf("stop touched another label: %v %v", err, calls)
	}
}

func TestDeployOpsRunTheCaddyNamedInDeployFile(t *testing.T) {
	skipDarwinOnlyDeploy(t)
	home := t.TempDir()
	caddy := filepath.Join(home, "caddy")
	if err := os.WriteFile(caddy, []byte("#!/bin/sh\necho '{\"from\":\"configured\"}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ops := newDeployOps(deployFile{CaddyAdmin: "127.0.0.1:1", Caddy: caddy, deployConfig: testDeployConfig(t)}, home, filepath.Join(home, "agents"), filepath.Join(home, "binary")).(*systemDeployOps)
	out, err := ops.adapt(t.Context(), filepath.Join(home, "Caddyfile"))
	if err != nil || !strings.Contains(string(out), "configured") {
		t.Fatalf("adapt ran another caddy: %q %v", out, err)
	}
}

func testDeployConfig(t *testing.T) deployConfig {
	t.Helper()
	address := func() string {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		return listener.Addr().String()
	}
	c := deployConfig{PublicAPI: address(), PublicUI: address(), BlueAPI: address(), BlueUI: address(), GreenAPI: address(), GreenUI: address()}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	return c
}
