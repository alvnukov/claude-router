package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// The installed agents were written before this renderer: the legacy one by
// the shell script, the slot and Caddy ones by the deploy command. An update
// that renders them from Go must leave them unchanged, except that the legacy
// agent now gets the slots' ExitTimeOut, so a stop lets it drain.
func TestLaunchdPlistMatchesInstalledAgents(t *testing.T) {
	home := "/home/router/.claude/local-router"
	for _, tc := range []struct {
		golden string
		spec   ServiceSpec
	}{
		{"launchd-com.claude-local-router.plist", ServiceSpec{Label: "com.claude-local-router", Exe: home + "/localrouter", Dir: home, LogPath: home + "/router.log", KeepAlive: true, ThrottleInterval: 10 * time.Second, ExitTimeout: 960 * time.Second}},
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

// exitCode is the error of a program that exited with a status, as
// *exec.ExitError is.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitCode) ExitCode() int { return int(e) }

// notFound is launchctl print's answer once launchd has no such agent.
var notFound = launchctlError([]string{"print"}, exitCode(launchctlNotFound), nil)

// fakeLaunchctl records launchctl calls; fail holds the error each failing
// verb returns. prints, when set, scripts print's answers in turn, the last
// one repeated: nil finds the agent. Like RunLaunchctl, it fails on a
// context that is over.
type fakeLaunchctl struct {
	calls  [][]string
	fail   map[string]error
	prints []error
}

func (f *fakeLaunchctl) run(ctx context.Context, args ...string) error {
	f.calls = append(f.calls, args)
	if err := ctx.Err(); err != nil {
		return launchctlError(args, err, nil)
	}
	if args[0] == "print" && len(f.prints) > 0 {
		err := f.prints[0]
		if len(f.prints) > 1 {
			f.prints = f.prints[1:]
		}
		return err
	}
	return f.fail[args[0]]
}

func (f *fakeLaunchctl) verbs() []string {
	var verbs []string
	for _, call := range f.calls {
		verbs = append(verbs, call[0])
	}
	return verbs
}

// newFakeLaunchd fails the named verbs as launchctl does: print with its
// "not found", anything else with a plain failure.
func newFakeLaunchd(t *testing.T, fail ...string) (Launchd, *fakeLaunchctl) {
	fake := &fakeLaunchctl{fail: map[string]error{}}
	for _, verb := range fail {
		fake.fail[verb] = launchctlError([]string{verb}, exitCode(5), nil)
		if verb == "print" {
			fake.fail[verb] = notFound
		}
	}
	return Launchd{Dir: filepath.Join(t.TempDir(), "LaunchAgents"), Domain: "gui/501", Poll: time.Nanosecond, Run: fake.run}, fake
}

// outlasted makes l wait an hour between prints and returns a context whose
// deadline passes while l waits, so a Stop sees the agent loaded once and
// then the end of its wait.
func outlasted(t *testing.T, l *Launchd) context.Context {
	l.Poll = time.Hour
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
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
	fake.prints = []error{nil, notFound}
	ctx := t.Context()
	if err := l.Start(ctx, "svc"); err != nil {
		t.Fatal(err)
	}
	st, err := l.Status(ctx, "svc")
	if err != nil || st != (Status{Loaded: true}) {
		t.Fatalf("Status = %+v, %v; want loaded, not installed", st, err)
	}
	if err := l.Stop(ctx, "svc"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"bootstrap", "gui/501", filepath.Join(l.Dir, "svc.plist")},
		{"print", "gui/501/svc"},
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
		name  string
		fail  []string
		stuck bool // the agent stays loaded until the wait for it is over
	}{
		{"loaded", nil, false},
		{"not loaded", []string{"bootout"}, false},
		{"stuck loaded", []string{"bootout"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, fake := newFakeLaunchd(t, tc.fail...)
			if err := l.Install(t.Context(), ServiceSpec{Label: "svc", Exe: "/bin/svc"}); err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			fake.prints = []error{notFound}
			if tc.stuck {
				ctx, fake.prints = outlasted(t, &l), []error{nil}
			}
			err := l.Uninstall(ctx, "svc")
			if (err != nil) != tc.stuck {
				t.Fatalf("Uninstall error = %v; want error %t", err, tc.stuck)
			}
			if verbs, want := fake.verbs(), []string{"bootout", "print"}; !reflect.DeepEqual(verbs, want) {
				t.Fatalf("launchctl verbs = %v; want %v", verbs, want)
			}
			// A plist whose agent could not be unloaded stays, so a retry
			// finds it; otherwise it is gone.
			_, statErr := os.Stat(filepath.Join(l.Dir, "svc.plist"))
			if kept := statErr == nil; kept != tc.stuck {
				t.Fatalf("plist kept = %t after %s", kept, tc.name)
			}
		})
	}
}

// bootout only starts an unload: launchd sends SIGTERM and keeps the agent
// until the program exits, which for a router draining its requests is up
// to its ExitTimeOut. Stop returns once print no longer finds the agent, so
// a start after it loads the agent again rather than find it loaded and
// leave the router down. An agent still loaded when the context ends is a
// failure, reported as such rather than as a print cut short, and so is a
// failed bootout only then: launchctl can report one while launchd unloads
// the agent.
func TestLaunchdStopWaitsForTheAgentToUnload(t *testing.T) {
	for _, tc := range []struct {
		name       string
		bootout    bool // bootout fails
		prints     []error
		outlasted  bool // the context ends while Stop waits
		wantErr    bool
		wantPrints int // prints after bootout
	}{
		{"unloads", false, []error{notFound}, false, false, 1},
		{"drains first", false, []error{nil, nil, nil, notFound}, false, false, 4},
		{"failed bootout, unloaded anyway", true, []error{notFound}, false, false, 1},
		{"outlasts the wait", false, []error{nil}, true, true, 1},
		{"failed bootout, still loaded", true, []error{nil}, true, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fail []string
			if tc.bootout {
				fail = []string{"bootout"}
			}
			l, fake := newFakeLaunchd(t, fail...)
			fake.prints = tc.prints
			ctx := t.Context()
			if tc.outlasted {
				ctx = outlasted(t, &l)
			}
			err := l.Stop(ctx, "svc")
			if (err != nil) != tc.wantErr {
				t.Fatalf("Stop = %v; want error %t", err, tc.wantErr)
			}
			if tc.wantErr && (!strings.Contains(fmt.Sprint(err), "launchd still has svc loaded") || !errors.Is(err, context.DeadlineExceeded) || tc.bootout && !errors.Is(err, fake.fail["bootout"])) {
				t.Fatalf("Stop = %v; want the agent still loaded at the deadline, and bootout's failure", err)
			}
			want := append([]string{"bootout"}, slices.Repeat([]string{"print"}, tc.wantPrints)...)
			if verbs := fake.verbs(); !reflect.DeepEqual(verbs, want) {
				t.Fatalf("launchctl verbs = %v; want %v", verbs, want)
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

// Only launchctl's "not found" says an agent is not loaded.
func TestLaunchctlNotFoundMeansNotLoaded(t *testing.T) {
	args := []string{"print", "gui/501/svc"}
	if err := launchctlError(args, exitCode(113), []byte("Could not find service")); !errors.Is(err, ErrNotLoaded) {
		t.Fatalf("exit 113 = %v; want ErrNotLoaded", err)
	}
	for _, cause := range []error{exitCode(5), context.DeadlineExceeded} {
		if err := launchctlError(args, cause, nil); errors.Is(err, ErrNotLoaded) || !errors.Is(err, cause) {
			t.Fatalf("%v became %v", cause, err)
		}
	}
}

// Any other failure of print, a timeout say, leaves the agent's state
// unknown: Status fails, and Uninstall keeps the plist of an agent that may
// still run.
func TestLaunchdFailureOtherThanNotFoundIsAnError(t *testing.T) {
	l, fake := newFakeLaunchd(t)
	fake.fail = map[string]error{"bootout": context.DeadlineExceeded, "print": context.DeadlineExceeded}
	if err := l.Install(t.Context(), ServiceSpec{Label: "svc", Exe: "/bin/svc"}); err != nil {
		t.Fatal(err)
	}
	if st, err := l.Status(t.Context(), "svc"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Status = %+v, %v; want the timeout", st, err)
	}
	if err := l.Uninstall(t.Context(), "svc"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Uninstall = %v; want the timeout", err)
	}
	if _, err := os.Stat(filepath.Join(l.Dir, "svc.plist")); err != nil {
		t.Fatalf("plist of an agent that may still run was removed: %v", err)
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
