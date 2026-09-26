package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// routerScript runs the router script in a fresh home with go, launchctl,
// curl, caddy and pkill stubbed, and a fake localrouter binary; stale makes
// that binary older than the script. calls lists the stubs' and the binary's
// invocations, with the home written as ~.
func routerScript(t *testing.T, command string, slot, stale bool) (string, string, error) {
	t.Helper()
	skipDarwinOnlyDeploy(t)
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	calls := filepath.Join(home, "calls")
	stub(filepath.Join(bin, "localrouter"), `printf 'binary %s\n' "$*" >> "$CALLS"`)
	stub(filepath.Join(bin, "go"), `printf 'go %s\n' "$*" >> "$CALLS"
while [ "$1" != "-o" ]; do shift; done
cp "$(dirname "$0")/localrouter" "$2"`)
	for _, name := range []string{"launchctl", "curl", "caddy", "pkill"} {
		stub(filepath.Join(bin, name), `printf '`+name+` %s\n' "$*" >> "$CALLS"`)
	}
	binary := filepath.Join(home, "localrouter")
	if err := exec.Command("cp", filepath.Join(bin, "localrouter"), binary).Run(); err != nil {
		t.Fatal(err)
	}
	if stale {
		if err := os.Chtimes(binary, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
			t.Fatal(err)
		}
	}
	ports := make([]string, 7)
	for i := range ports {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ports[i] = listener.Addr().String()
		listener.Close()
	}
	if err := os.WriteFile(filepath.Join(home, "env"), []byte(fmt.Sprintf("ROUTER_LISTEN=%s\nROUTER_UI_LISTEN=%s\nROUTER_BLUE_API=%s\nROUTER_BLUE_UI=%s\nROUTER_GREEN_API=%s\nROUTER_GREEN_UI=%s\nROUTER_CADDY_ADMIN=%s\n", ports[0], ports[1], ports[2], ports[3], ports[4], ports[5], ports[6])), 0o600); err != nil {
		t.Fatal(err)
	}
	if slot {
		if err := os.WriteFile(filepath.Join(home, "deploy.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "active-slot"), []byte("blue\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", append([]string{"router"}, strings.Fields(command)...)...) // commands contain no quoted arguments here; all values are in env.
	cmd.Env = append(os.Environ(), "ROUTER_HOME="+home, "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"), "CALLS="+calls)
	output, err := cmd.CombinedOutput()
	data, _ := os.ReadFile(calls)
	return string(output), strings.ReplaceAll(string(data), home, "~"), err
}

func TestRouterInstallCutoverPassesConfiguredAddressesAndCaddyPath(t *testing.T) {
	output, calls, err := routerScript(t, "install --cutover", false, false)
	if err != nil {
		t.Fatalf("install cutover: %v %s %s", err, output, calls)
	}
	for _, expected := range []string{"binary cutover -home", "-public-api 127.0.0.1:", "-blue-api 127.0.0.1:", "-green-ui 127.0.0.1:", "-caddy-admin 127.0.0.1:", "-caddy "} {
		if !strings.Contains(calls, expected) {
			t.Fatalf("cutover missing %q: %s", expected, calls)
		}
	}
	if strings.Contains(calls, "launchctl") || strings.Contains(calls, "curl") {
		t.Fatalf("shell took a public port before cutover confirmation: %s", calls)
	}
}

func TestRouterRestartUsesForcedDeployAfterCutover(t *testing.T) {
	output, calls, err := routerScript(t, "restart", true, false)
	if err != nil {
		t.Fatalf("restart: %v %s %s", err, output, calls)
	}
	if !strings.Contains(calls, "binary deploy -home") || !strings.Contains(calls, " -force") || strings.Contains(calls, "launchctl kickstart") {
		t.Fatalf("slot restart was not a forced deploy: %s", calls)
	}
}

// The binary manages the agent; the script only builds it and passes the
// command on, and restart is stop then start.
func TestRouterHandsServiceCommandsToTheBinary(t *testing.T) {
	for _, tc := range []struct{ command, want string }{
		{"", "binary status -home ~\n"},
		{"start", "binary start -home ~\n"},
		{"stop", "binary stop -home ~\n"},
		{"status", "binary status -home ~\n"},
		{"uninstall", "binary uninstall -home ~\n"},
		{"env", "binary env -home ~\n"},
		{"install", "go build -o ~/localrouter .\nbinary install -home ~\n"},
		{"restart", "go build -o ~/localrouter .\nbinary stop -home ~\nbinary start -home ~\n"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			output, calls, err := routerScript(t, tc.command, false, false)
			if err != nil || calls != tc.want {
				t.Fatalf("router %s: %v, output %q, calls %q; want calls %q", tc.command, err, output, calls, tc.want)
			}
		})
	}
}

// A binary from before the command table would take status or stop for
// serve, so only start and install, which build first, run with one.
func TestRouterRefusesABinaryOlderThanTheScript(t *testing.T) {
	for _, tc := range []struct{ command, want string }{
		{"start", "go build -o ~/localrouter .\nbinary start -home ~\n"},
		{"install", "go build -o ~/localrouter .\nbinary install -home ~\n"},
		{"stop", ""},
		{"status", ""},
		{"uninstall", ""},
		{"env", ""},
	} {
		t.Run(tc.command, func(t *testing.T) {
			output, calls, err := routerScript(t, tc.command, false, true)
			if calls != tc.want {
				t.Fatalf("router %s: calls %q; want %q (output %q)", tc.command, calls, tc.want, output)
			}
			if refused := tc.want == ""; refused != (err != nil) || refused != strings.Contains(output, "run ./router build") {
				t.Fatalf("router %s: err %v, output %q", tc.command, err, output)
			}
		})
	}
}
