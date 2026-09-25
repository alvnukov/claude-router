package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func routerScript(t *testing.T, command string, slot bool) (string, string, error) {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	calls := filepath.Join(home, "calls")
	stub("go", `printf 'go %s\n' "$*" >> "$CALLS"
while [ "$1" != "-o" ]; do shift; done
shift
printf '#!/bin/sh\nprintf "binary %%s\\n" "$*" >> "$CALLS"\n' > "$1"
chmod +x "$1"`)
	stub("launchctl", `printf 'launchctl %s\n' "$*" >> "$CALLS"; [ "$1" != print ]`)
	stub("curl", `printf 'curl %s\n' "$*" >> "$CALLS"; exit 0`)
	stub("caddy", `printf 'caddy %s\n' "$*" >> "$CALLS"`)
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
		for _, label := range []string{"com.claude-local-router.caddy", "com.claude-local-router.blue"} {
			plist := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
			if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(plist, []byte("fixture"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
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
	return string(output), string(data), err
}

func TestRouterInstallCutoverPassesConfiguredAddressesAndCaddyPath(t *testing.T) {
	output, calls, err := routerScript(t, "install --cutover", false)
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
	output, calls, err := routerScript(t, "restart", true)
	if err != nil {
		t.Fatalf("restart: %v %s %s", err, output, calls)
	}
	if !strings.Contains(calls, "binary deploy -home") || !strings.Contains(calls, " -force") || strings.Contains(calls, "launchctl kickstart") {
		t.Fatalf("slot restart was not a forced deploy: %s", calls)
	}
}

func TestRouterSlotStartStopAndStatusUseCaddyLabel(t *testing.T) {
	for _, tc := range []struct{ command, want string }{
		{"start", "launchctl bootstrap gui/"},
		{"stop", "com.claude-local-router.caddy"},
		{"status", "com.claude-local-router.caddy"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			output, calls, err := routerScript(t, tc.command, true)
			if err != nil || !strings.Contains(calls+output, tc.want) {
				t.Fatalf("%s: output %s calls %s err %v", tc.command, output, calls, err)
			}
		})
	}
}
