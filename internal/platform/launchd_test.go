package platform

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The installed agents were written before this renderer: the legacy one by
// the shell script, the slot and Caddy ones by the deploy command. An update
// that renders them from Go must leave them unchanged.
func TestLaunchdPlistMatchesInstalledAgents(t *testing.T) {
	home := "/home/router/.claude/local-router"
	for _, tc := range []struct {
		golden string
		spec   ServiceSpec
	}{
		{"launchd-com.claude-local-router.plist", ServiceSpec{Label: "com.claude-local-router", Exe: home + "/localrouter", Dir: home, LogPath: home + "/router.log", KeepAlive: true, ThrottleInterval: 10 * time.Second}},
		{"launchd-com.claude-local-router.green.plist", ServiceSpec{Label: "com.claude-local-router.green", Exe: home + "/localrouter.green", Dir: home, LogPath: home + "/router.green.log", KeepAlive: true, ExitTimeout: 960 * time.Second,
			Env: map[string]string{"ROUTER_SLOT": "green", "ROUTER_ACTIVE_SLOT_FILE": home + "/active-slot", "ROUTER_LISTEN": "127.0.0.1:18792", "ROUTER_PUBLIC_LISTEN": "127.0.0.1:18787", "ROUTER_UI_LISTEN": "127.0.0.1:18794", "ROUTER_PROVIDERS_FILE": home + "/providers.json", "ROUTER_ANTHROPIC_LIMITS_FILE": home + "/limits.json", "ROUTER_ENV_FILE": home + "/env", "ROUTER_STATE_FILE": home + "/state.json", "ROUTER_UI_HISTORY_FILE": home + "/history.jsonl"}}},
		{"launchd-com.claude-local-router.caddy.plist", ServiceSpec{Label: "com.claude-local-router.caddy", Exe: "/opt/homebrew/bin/caddy", Args: []string{"run", "--config", home + "/Caddyfile", "--adapter", "caddyfile"}, Dir: home, LogPath: home + "/caddy.log", KeepAlive: true, ExitTimeout: 30 * time.Second,
			Env: map[string]string{"XDG_DATA_HOME": home + "/caddy/data", "XDG_CONFIG_HOME": home + "/caddy/config"}}},
	} {
		want, err := os.ReadFile(filepath.Join("testdata", tc.golden))
		if err != nil {
			t.Fatal(err)
		}
		if got := LaunchdPlist(tc.spec); !bytes.Equal(got, want) {
			t.Fatalf("%s differs:\n--- got\n%s\n--- want\n%s", tc.golden, got, want)
		}
	}
}

func TestLaunchdPlistWritesOptionalKeysOnlyWhenSet(t *testing.T) {
	spec := ServiceSpec{Label: "l", Exe: "/bin/x", Args: []string{"a&b"}, Dir: "/d", LogPath: "/d/log", Env: map[string]string{"B": "2", "A": "<1>"}, ExitTimeout: 960 * time.Second}
	got := string(LaunchdPlist(spec))
	for _, want := range []string{
		"<array><string>/bin/x</string><string>a&amp;b</string></array>",
		"<key>KeepAlive</key><false/>",
		"<key>ExitTimeOut</key><integer>960</integer>",
		"<key>A</key><string>&lt;1&gt;</string>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("plist lacks %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ThrottleInterval") {
		t.Fatalf("zero ThrottleInterval was written:\n%s", got)
	}
	if strings.Index(got, "<key>A</key>") > strings.Index(got, "<key>B</key>") {
		t.Fatalf("environment is not sorted:\n%s", got)
	}
	bare := string(LaunchdPlist(ServiceSpec{Label: "l", Exe: "/bin/x"}))
	for _, absent := range []string{"ExitTimeOut", "EnvironmentVariables"} {
		if strings.Contains(bare, absent) {
			t.Fatalf("unset %s was written:\n%s", absent, bare)
		}
	}
}

// fakeLaunchctl records launchctl calls; fail names the verbs that exit
// non-zero.
type fakeLaunchctl struct {
	calls [][]string
	fail  map[string]bool
}

func (f *fakeLaunchctl) run(_ context.Context, args ...string) error {
	f.calls = append(f.calls, args)
	if f.fail[args[0]] {
		return errors.New("launchctl " + args[0] + " failed")
	}
	return nil
}

func newFakeLaunchd(t *testing.T, fail ...string) (Launchd, *fakeLaunchctl) {
	fake := &fakeLaunchctl{fail: map[string]bool{}}
	for _, verb := range fail {
		fake.fail[verb] = true
	}
	return Launchd{Dir: filepath.Join(t.TempDir(), "LaunchAgents"), Domain: "gui/501", Run: fake.run}, fake
}

func TestLaunchdInstallWritesPlistWithoutLoading(t *testing.T) {
	l, fake := newFakeLaunchd(t)
	spec := ServiceSpec{Label: "com.example.svc", Exe: "/bin/svc", Dir: "/d", LogPath: "/d/log", KeepAlive: true}
	if err := l.Install(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(l.Dir, "com.example.svc.plist")
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, LaunchdPlist(spec)) {
		t.Fatalf("plist %s = %q, %v", path, got, err)
	}
	if runtime.GOOS != "windows" {
		for p, want := range map[string]os.FileMode{path: 0o644, l.Dir: 0o755 | os.ModeDir} {
			if info, err := os.Stat(p); err != nil || info.Mode() != want {
				t.Fatalf("%s mode = %v, %v; want %v", p, info.Mode(), err, want)
			}
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("Install ran launchctl: %v", fake.calls)
	}
}

func TestLaunchdStartStopAndStatusCallLaunchctl(t *testing.T) {
	l, fake := newFakeLaunchd(t)
	ctx := t.Context()
	if err := l.Start(ctx, "svc"); err != nil {
		t.Fatal(err)
	}
	if err := l.Stop(ctx, "svc"); err != nil {
		t.Fatal(err)
	}
	st, err := l.Status(ctx, "svc")
	if err != nil || st != (Status{Loaded: true}) {
		t.Fatalf("Status = %+v, %v; want loaded, not installed", st, err)
	}
	want := [][]string{
		{"bootstrap", "gui/501", filepath.Join(l.Dir, "svc.plist")},
		{"bootout", "gui/501/svc"},
		{"print", "gui/501/svc"},
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("launchctl calls = %v; want %v", fake.calls, want)
	}
}

func TestLaunchdStatusReportsInstalledButNotLoaded(t *testing.T) {
	l, _ := newFakeLaunchd(t, "print")
	if err := l.Install(t.Context(), ServiceSpec{Label: "svc", Exe: "/bin/svc"}); err != nil {
		t.Fatal(err)
	}
	st, err := l.Status(t.Context(), "svc")
	if err != nil || st != (Status{Installed: true}) {
		t.Fatalf("Status = %+v, %v; want installed, not loaded", st, err)
	}
}

func TestLaunchdUninstall(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fail      []string
		wantCalls []string
		wantErr   bool
	}{
		{"loaded", nil, []string{"bootout"}, false},
		{"not loaded", []string{"bootout", "print"}, []string{"bootout", "print"}, false},
		{"stuck loaded", []string{"bootout"}, []string{"bootout", "print"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, fake := newFakeLaunchd(t, tc.fail...)
			if err := l.Install(t.Context(), ServiceSpec{Label: "svc", Exe: "/bin/svc"}); err != nil {
				t.Fatal(err)
			}
			err := l.Uninstall(t.Context(), "svc")
			if (err != nil) != tc.wantErr {
				t.Fatalf("Uninstall error = %v; want error %t", err, tc.wantErr)
			}
			var verbs []string
			for _, call := range fake.calls {
				verbs = append(verbs, call[0])
			}
			if !reflect.DeepEqual(verbs, tc.wantCalls) {
				t.Fatalf("launchctl verbs = %v; want %v", verbs, tc.wantCalls)
			}
			// A plist whose agent could not be unloaded stays, so a retry
			// finds it; otherwise it is gone.
			_, statErr := os.Stat(filepath.Join(l.Dir, "svc.plist"))
			if kept := statErr == nil; kept != tc.wantErr {
				t.Fatalf("plist kept = %t after %s", kept, tc.name)
			}
		})
	}
}

func TestLaunchdUninstallWithoutPlist(t *testing.T) {
	l, _ := newFakeLaunchd(t, "bootout", "print")
	if err := l.Uninstall(t.Context(), "svc"); err != nil {
		t.Fatalf("Uninstall of an absent agent = %v", err)
	}
}

func TestRunLaunchctlRefusesUnderTest(t *testing.T) {
	bin := t.TempDir()
	ran := filepath.Join(bin, "ran")
	if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte("#!/bin/sh\n/usr/bin/touch "+ran+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	err := RunLaunchctl(t.Context(), "bootout", "gui/0/com.claude-local-router.blue")
	if _, statErr := os.Stat(ran); err == nil || statErr == nil {
		t.Fatalf("launchctl ran under test: err=%v", err)
	}
}
