package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"localrouter/internal/platform"
)

func writeDeployFile(t *testing.T, home string, cfg deployConfig) {
	t.Helper()
	data, err := json.Marshal(deployFile{CaddyAdmin: "127.0.0.1:1", deployConfig: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "deploy.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func deployBinary(t *testing.T, home string) string {
	t.Helper()
	path := filepath.Join(home, "localrouter.candidate")
	if err := os.WriteFile(path, []byte("candidate"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestServiceLabelsReadDeployFileWithDefault(t *testing.T) {
	for _, prefix := range []string{"", "com.claude-local-router.scratch-123"} {
		home := t.TempDir()
		file := deployFile{CaddyAdmin: "127.0.0.1:1", LabelPrefix: prefix, deployConfig: testDeployConfig(t)}
		data, err := json.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "deploy.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := runServiceLabels([]string{"-home", home}, &out); err != nil {
			t.Fatal(err)
		}
		var labels struct{ Blue, Green, Caddy string }
		if err := json.Unmarshal(out.Bytes(), &labels); err != nil {
			t.Fatal(err)
		}
		want := prefix
		if want == "" {
			want = defaultRouterLabel
		}
		if labels.Blue != want+".blue" || labels.Green != want+".green" || labels.Caddy != want+".caddy" {
			t.Fatalf("prefix %q labels %+v", prefix, labels)
		}
	}
}

func TestDeployFileRejectsInvalidLabelPrefix(t *testing.T) {
	cfg := testDeployConfig(t)
	for _, label := range []string{"../router", "com.example/router", "com.example router", "com.example.", "com.claude-local-router.caddy"} {
		file := deployFile{CaddyAdmin: "127.0.0.1:1", LabelPrefix: label, deployConfig: cfg}
		if err := file.validate(); err == nil {
			t.Fatalf("accepted label prefix %q", label)
		}
	}
	if err := (deployFile{CaddyAdmin: "127.0.0.1:1", LabelPrefix: "com.claude-local-router.check-123", deployConfig: cfg}).validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDeployCommandRunsControllerWithConfiguredPorts(t *testing.T) {
	f := newDeployFixture(t)
	home := t.TempDir()
	writeDeployFile(t, home, f.controller.config)
	binary := deployBinary(t, home)
	var got deployFile
	var out bytes.Buffer
	err := runDeploy(t.Context(), []string{"-home", home, "-binary", binary}, &out, func(file deployFile, h, agents, b string) deployOps {
		got = file
		return f
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.deployConfig != f.controller.config || got.CaddyAdmin != "127.0.0.1:1" {
		t.Fatalf("controller built from other ports: %+v", got)
	}
	if f.active != "green" || !strings.Contains(out.String(), "green") {
		t.Fatalf("deploy did not switch: active=%s out=%q", f.active, out.String())
	}
}

func TestDeployCommandNeedsCutoverConfig(t *testing.T) {
	home := t.TempDir()
	built := false
	err := runDeploy(t.Context(), []string{"-home", home, "-binary", deployBinary(t, home)}, &bytes.Buffer{}, func(deployFile, string, string, string) deployOps {
		built = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "install --cutover") || built {
		t.Fatalf("deploy without slot config: built=%v err=%v", built, err)
	}
}

func TestDeployCommandRefusesConcurrentDeploy(t *testing.T) {
	f := newDeployFixture(t)
	home := t.TempDir()
	writeDeployFile(t, home, f.controller.config)
	binary := deployBinary(t, home)
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error)
	go func() {
		done <- platform.WithLock(context.Background(), filepath.Join(home, "deploy.lock"), func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	err := runDeploy(t.Context(), []string{"-home", home, "-binary", binary}, &bytes.Buffer{}, func(deployFile, string, string, string) deployOps { return f })
	close(release)
	if lockErr := <-done; lockErr != nil {
		t.Fatal(lockErr)
	}
	if err == nil || !strings.Contains(err.Error(), "another deploy") || len(f.calls) != 0 {
		t.Fatalf("second deploy ran alongside the first: err=%v calls=%v", err, f.calls)
	}
}

func TestLaunchctlNeverRunsUnderTest(t *testing.T) {
	bin := t.TempDir()
	ran := filepath.Join(bin, "ran")
	if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte("#!/bin/sh\n/usr/bin/touch "+ran+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	err := runLaunchctl(t.Context(), "bootout", "gui/0/com.claude-local-router.blue")
	if _, statErr := os.Stat(ran); err == nil || statErr == nil {
		t.Fatalf("launchctl ran under test: err=%v", err)
	}
}

func TestDeployCommandSaysWhenNothingChanged(t *testing.T) {
	f := newDeployFixture(t)
	home := t.TempDir()
	writeDeployFile(t, home, f.controller.config)
	binary := deployBinary(t, home)
	digest, err := fileDigest(binary)
	if err != nil {
		t.Fatal(err)
	}
	f.slots["blue"].Digest = digest
	var out bytes.Buffer
	if err := runDeploy(t.Context(), []string{"-home", home, "-binary", binary}, &out, func(deployFile, string, string, string) deployOps { return f }); err != nil {
		t.Fatal(err)
	}
	if f.active != "blue" || !strings.Contains(out.String(), "no change") {
		t.Fatalf("repeat deploy did not report a no-op: active=%s out=%q", f.active, out.String())
	}
}

func TestDeployCommandWaitsForASlowSlot(t *testing.T) {
	f := newDeployFixture(t)
	home := t.TempDir()
	writeDeployFile(t, home, f.controller.config)
	refusals := 3
	f.onState = func(slot string, s *deploySlotState) error {
		if slot == "green" && s.PID == 43 && refusals > 0 {
			refusals--
			return syscall.ECONNREFUSED
		}
		return nil
	}
	if err := runDeploy(t.Context(), []string{"-home", home, "-binary", deployBinary(t, home)}, &bytes.Buffer{}, func(deployFile, string, string, string) deployOps { return f }); err != nil || f.active != "green" {
		t.Fatalf("deploy gave up on a starting slot: %v, active %s", err, f.active)
	}
}

func TestServiceLabelsNamesProposedPrefixWithoutDeployFile(t *testing.T) {
	for _, tc := range []struct {
		prefix, blue string
		live         bool
	}{
		{"com.claude-local-router.check-1", "com.claude-local-router.check-1.blue", false},
		{defaultRouterLabel, defaultRouterLabel + ".blue", true},
	} {
		var out bytes.Buffer
		if err := runServiceLabels([]string{"-home", t.TempDir(), "-prefix", tc.prefix}, &out); err != nil {
			t.Fatal(err)
		}
		var labels struct {
			Blue          string
			DefaultPrefix bool `json:"default_prefix"`
		}
		if err := json.Unmarshal(out.Bytes(), &labels); err != nil {
			t.Fatal(err)
		}
		if labels.Blue != tc.blue || labels.DefaultPrefix != tc.live {
			t.Fatalf("prefix %q: %s", tc.prefix, out.String())
		}
	}
	if err := runServiceLabels([]string{"-prefix", "../router"}, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted an invalid proposed prefix")
	}
}
