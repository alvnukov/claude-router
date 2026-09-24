package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemStartAndStopUsesLaunchdSlotPlist(t *testing.T) {
	cfg := testDeployConfig(t)
	home := t.TempDir()
	agents := filepath.Join(home, "agents")
	binary := filepath.Join(home, "candidate")
	if err := os.WriteFile(binary, []byte("candidate"), 0700); err != nil {
		t.Fatal(err)
	}
	ops := newSystemDeployOps(cfg, "http://127.0.0.1:1", home, agents, binary)
	var commands [][]string
	ops.launchctl = func(_ context.Context, args ...string) error { commands = append(commands, args); return nil }
	sum := sha256.Sum256([]byte("candidate"))
	if err := ops.start(t.Context(), "green", hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(agents, "com.claude-local-router.green.plist"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ExitTimeOut", "960", filepath.Join(home, "localrouter.green"), cfg.GreenAPI, cfg.GreenUI, "ROUTER_ACTIVE_SLOT_FILE", "ROUTER_SLOT"} {
		if !contains(string(content), key) {
			t.Fatalf("slot plist omitted %s: %s", key, content)
		}
	}
	if installed, err := os.ReadFile(filepath.Join(home, "localrouter.green")); err != nil || string(installed) != "candidate" {
		t.Fatalf("slot binary not installed: %v", err)
	}
	if err := ops.start(t.Context(), "blue", "wrong"); err == nil {
		t.Fatal("started binary with mismatched digest")
	}
	if len(commands) != 1 || len(commands[0]) == 0 || commands[0][0] != "bootstrap" {
		t.Fatalf("unexpected start commands: %v", commands)
	}
	if err := ops.stop(t.Context(), "green"); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || commands[1][0] != "bootout" {
		t.Fatalf("unexpected stop commands: %v", commands)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
