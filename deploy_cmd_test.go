package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		done <- withFileLock(context.Background(), filepath.Join(home, "deploy.lock"), func() error {
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
