package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"localrouter/internal/platform"
)

const legacyLabel = "com.claude-local-router"

// fakeService keeps the state of each label in memory and records calls as
// "Verb label". started runs on every Start: loading an agent starts the
// router behind it.
type fakeService struct {
	state   map[string]platform.Status
	specs   map[string]platform.ServiceSpec
	calls   []string
	started func()
	stopErr error // Stop fails with it and leaves the agent loaded
	// waits holds, for each Stop and Uninstall, how long its context lets
	// it wait for the program to exit; zero if unbounded.
	waits []time.Duration
}

func (f *fakeService) record(verb, label string) { f.calls = append(f.calls, verb+" "+label) }

func (f *fakeService) wait(ctx context.Context) {
	var wait time.Duration
	if deadline, ok := ctx.Deadline(); ok {
		wait = time.Until(deadline)
	}
	f.waits = append(f.waits, wait)
}

func (f *fakeService) Install(_ context.Context, spec platform.ServiceSpec) error {
	f.record("Install", spec.Label)
	f.specs[spec.Label] = spec
	st := f.state[spec.Label]
	st.Installed = true
	f.state[spec.Label] = st
	return nil
}

func (f *fakeService) Uninstall(ctx context.Context, label string) error {
	f.wait(ctx)
	f.record("Uninstall", label)
	delete(f.state, label)
	return nil
}

func (f *fakeService) Start(_ context.Context, label string) error {
	f.record("Start", label)
	st := f.state[label]
	st.Loaded = true
	f.state[label] = st
	f.started()
	return nil
}

func (f *fakeService) Stop(ctx context.Context, label string) error {
	f.wait(ctx)
	f.record("Stop", label)
	if f.stopErr != nil {
		return f.stopErr
	}
	st := f.state[label]
	st.Loaded = false
	f.state[label] = st
	return nil
}

func (f *fakeService) Status(_ context.Context, label string) (platform.Status, error) {
	f.record("Status", label)
	return f.state[label], nil
}

// fixture is a router home with the machine faked around it.
type fixture struct {
	home     string
	svc      *fakeService
	launchd  *platform.Launchd // the service instead of svc, when set
	alive    bool              // /healthz answers
	upOnLoad bool              // /healthz answers once something starts the router
	probes   int
	health   []string
	spawned  []string
	killed   []string
	stopped  []int
	claude   string
	labels   error  // service-labels fails with this
	labelOut string // service-labels prints this instead of all three labels
}

func newFixture(t *testing.T) *fixture {
	t.Setenv("ROUTER_LISTEN", "")
	return &fixture{home: t.TempDir(), svc: &fakeService{state: map[string]platform.Status{}, specs: map[string]platform.ServiceSpec{}}, upOnLoad: true}
}

func (f *fixture) write(t *testing.T, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.home, name), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// slotMode makes the home one that install --cutover left behind; the
// fake service-labels names its agents p.blue, p.green and p.caddy.
func (f *fixture) slotMode(t *testing.T, active string) {
	f.write(t, "deploy.json", "{}\n")
	if active != "" {
		f.write(t, "active-slot", active)
	}
}

func (f *fixture) commands() []Entry {
	var svc platform.Service = f.svc
	if f.launchd != nil {
		svc = f.launchd
	}
	host := Host{
		Service: svc,
		Healthy: func(_ context.Context, url string) bool {
			f.probes++
			f.health = append(f.health, url)
			return f.alive
		},
		Spawn: func(exe string, args []string, logPath string) (int, error) {
			f.spawned = append(f.spawned, fmt.Sprint(exe, args, logPath))
			f.alive = f.upOnLoad
			return 4242, nil
		},
		Terminate:    func(pid int) error { f.stopped = append(f.stopped, pid); return nil },
		KillMatching: func(pattern string) error { f.killed = append(f.killed, pattern); return nil },
	}
	f.svc.started = func() { f.alive = f.upOnLoad }
	router := Router{
		Label: legacyLabel,
		SlotLabels: func(args []string, out io.Writer) error {
			if !reflect.DeepEqual(args, []string{"-home", f.home}) {
				return fmt.Errorf("service-labels called with %q", args)
			}
			if f.labels != nil {
				return f.labels
			}
			labels := `{"blue":"p.blue","caddy":"p.caddy","default_prefix":false,"green":"p.green","prefix":"p"}`
			if f.labelOut != "" {
				labels = f.labelOut
			}
			_, err := io.WriteString(out, labels+"\n")
			return err
		},
		ClientURL: func(listen string) (string, error) {
			if listen == "" {
				listen = "127.0.0.1:8787"
			}
			return "http://" + listen, nil
		},
		SetClaudeURL: func(url string) error { f.claude = url; return nil },
		ReadEnv: func(path string) (map[string]string, error) {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			vals := map[string]string{}
			for _, line := range strings.Fields(string(data)) {
				k, v, _ := strings.Cut(line, "=")
				vals[k] = v
			}
			return vals, nil
		},
	}
	return Commands(router, host)
}

func (f *fixture) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args = append(args, "-home", f.home)
	code := Dispatch(t.Context(), f.commands(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func expect(t *testing.T, code int, stdout, stderr string, wantCode int, wantStdout, wantStderr string) {
	t.Helper()
	if code != wantCode || stdout != wantStdout || stderr != wantStderr {
		t.Fatalf("exit %d, stdout %q, stderr %q; want %d, %q, %q", code, stdout, stderr, wantCode, wantStdout, wantStderr)
	}
}

func expectCalls(t *testing.T, f *fixture, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(f.svc.calls, want) && (len(f.svc.calls) != 0 || len(want) != 0) {
		t.Fatalf("service calls = %q; want %q", f.svc.calls, want)
	}
}

func TestCommandsTable(t *testing.T) {
	var names []string
	for _, entry := range newFixture(t).commands() {
		names = append(names, entry.Name)
	}
	if want := []string{"install", "uninstall", "start", "stop", "status", "env"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("commands = %q; want %q", names, want)
	}
}

func TestCommandsRequireHome(t *testing.T) {
	f := newFixture(t)
	var stdout, stderr bytes.Buffer
	if code := Dispatch(t.Context(), f.commands(), []string{"status"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "-home") {
		t.Fatalf("status without -home: exit %d, stderr %q", code, stderr.String())
	}
	expectCalls(t, f)
}

// The legacy agent keeps the plist the shell script wrote, byte for byte,
// but for ExitTimeOut, which lets it drain its requests when stopped.
func TestLegacySpecMatchesInstalledAgent(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "platform", "testdata", "launchd-com.claude-local-router.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if got := platform.LaunchdPlist(legacySpec(legacyLabel, "/home/router/.claude/local-router")); !bytes.Equal(got, want) {
		t.Fatalf("legacy plist differs:\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func TestInstallStopsTheHandStartedRouterAndLoadsTheAgent(t *testing.T) {
	f := newFixture(t)
	f.write(t, "router.pid", "77\n")
	code, stdout, stderr := f.run(t, "install")
	expect(t, code, stdout, stderr, 0, "installed as com.claude-local-router, listening on 127.0.0.1:8787\n", "")
	expectCalls(t, f, "Install "+legacyLabel, "Status "+legacyLabel, "Start "+legacyLabel)
	if got := f.svc.specs[legacyLabel]; !reflect.DeepEqual(got, legacySpec(legacyLabel, f.home)) {
		t.Fatalf("installed %+v", got)
	}
	// The pattern also catches an instance claude-local started without a
	// pidfile, and is anchored so the slot binaries survive.
	if want := []string{regexp.QuoteMeta(f.home+"/localrouter") + "( |$)"}; !reflect.DeepEqual(f.killed, want) {
		t.Fatalf("killed %q; want %q", f.killed, want)
	}
	if !reflect.DeepEqual(f.stopped, []int{77}) {
		t.Fatalf("terminated %v; want the pidfile's 77", f.stopped)
	}
	if _, err := os.Stat(filepath.Join(f.home, "router.pid")); !os.IsNotExist(err) {
		t.Fatalf("pidfile left behind: %v", err)
	}
	if !reflect.DeepEqual(f.health[0], "http://127.0.0.1:8787/healthz") {
		t.Fatalf("probed %q", f.health)
	}
}

// Over a loaded agent install unloads and loads it again, which restarts
// the router on the new plist. launchctl kickstart -k is not used.
func TestInstallOverLoadedAgentIsStopThenStart(t *testing.T) {
	f := newFixture(t)
	f.svc.state[legacyLabel] = platform.Status{Installed: true, Loaded: true}
	code, stdout, stderr := f.run(t, "install")
	expect(t, code, stdout, stderr, 0, "installed as com.claude-local-router, listening on 127.0.0.1:8787\n", "")
	expectCalls(t, f, "Install "+legacyLabel, "Status "+legacyLabel, "Stop "+legacyLabel, "Start "+legacyLabel)
}

// Whether a failed bootout still unloaded the agent is the Service's to
// decide. A Stop that fails means the router may still run, so the command
// fails there: install does not load the agent again, and stop does not
// report the router stopped.
func TestFailedStopFailsTheCommand(t *testing.T) {
	for _, tc := range []struct {
		name      string
		slot      bool
		command   string
		wantCalls []string
	}{
		{"install", false, "install", []string{"Install " + legacyLabel, "Status " + legacyLabel, "Stop " + legacyLabel}},
		{"stop", false, "stop", []string{"Status " + legacyLabel, "Stop " + legacyLabel}},
		{"slot stop", true, "stop", []string{"Status p.caddy", "Stop p.caddy"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.slot {
				f.slotMode(t, "blue\n")
				f.svc.state["p.caddy"] = platform.Status{Installed: true, Loaded: true}
			} else {
				f.svc.state[legacyLabel] = platform.Status{Installed: true, Loaded: true}
			}
			f.svc.stopErr = errors.New("Boot-out failed: 5: Input/output error")
			code, stdout, stderr := f.run(t, tc.command)
			expect(t, code, stdout, stderr, 1, "", "Boot-out failed: 5: Input/output error\n")
			expectCalls(t, f, tc.wantCalls...)
		})
	}
}

// When launchctl cannot tell whether bootout unloaded the agent, a timeout
// say, stop fails: the router may still run.
func TestStopFailsWhenLaunchdCannotTellTheAgentUnloaded(t *testing.T) {
	f, prints := newFixture(t), 0
	f.launchd = &platform.Launchd{Dir: t.TempDir(), Domain: "gui/501", Run: func(_ context.Context, args ...string) error {
		if args[0] == "print" && prints == 0 {
			prints++ // the first print, before bootout, finds the agent
			return nil
		}
		return context.DeadlineExceeded
	}}
	if err := os.WriteFile(filepath.Join(f.launchd.Dir, legacyLabel+".plist"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := f.run(t, "stop")
	if code != 1 || stdout != "" || !strings.Contains(stderr, context.DeadlineExceeded.Error()) {
		t.Fatalf("stop = exit %d, stdout %q, stderr %q; want exit 1 with the timeout", code, stdout, stderr)
	}
}

// A router drains its requests after SIGTERM, and launchd keeps its agent
// loaded until it exits. restart is stop, then start: stop returns once the
// agent is gone, so start loads it again. Had stop returned at bootout,
// start would find the agent loaded, not load it and leave the router down.
func TestRestartWhileTheRouterDrains(t *testing.T) {
	f := newFixture(t)
	f.alive = true
	state, drainPrints := "running", 3 // prints that find the agent while it drains
	var verbs []string
	f.launchd = &platform.Launchd{Dir: t.TempDir(), Domain: "gui/501", Poll: time.Nanosecond, Run: func(_ context.Context, args ...string) error {
		verbs = append(verbs, args[0])
		switch args[0] {
		case "bootout":
			state, f.alive = "draining", false // a draining router takes no new requests
		case "bootstrap":
			state, f.alive = "running", true
		case "print":
			if state == "draining" {
				if drainPrints == 0 {
					state = "gone"
				}
				drainPrints--
			}
			if state == "gone" {
				return fmt.Errorf("launchctl print: %w", platform.ErrNotLoaded)
			}
		}
		return nil
	}}
	if err := os.WriteFile(filepath.Join(f.launchd.Dir, legacyLabel+".plist"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := f.run(t, "stop")
	expect(t, code, stdout, stderr, 0, "stopped; launchd will start it again at next login or on 'router start'\n", "")
	code, stdout, stderr = f.run(t, "start")
	expect(t, code, stdout, stderr, 0, "started by launchd on 127.0.0.1:8787\n", "")
	want := []string{"print", "bootout", "print", "print", "print", "print", "print", "bootstrap"}
	if !reflect.DeepEqual(verbs, want) {
		t.Fatalf("launchctl verbs = %v; want %v", verbs, want)
	}
}

// A router may drain for up to its agent's ExitTimeOut, 960 s, before launchd
// kills it, and Stop waits for that. Every wait the commands start is
// bounded, and longer than that.
func TestStopWaitsAsLongAsTheRouterMayDrain(t *testing.T) {
	const exitTimeOut = 960 * time.Second
	for _, tc := range []struct {
		name    string
		slot    bool
		command string
	}{
		{"install", false, "install"},
		{"stop", false, "stop"},
		{"uninstall", false, "uninstall"},
		{"slot stop", true, "stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			labels := []string{legacyLabel}
			if tc.slot {
				f.slotMode(t, "blue\n")
				labels = []string{"p.caddy", "p.blue", "p.green"}
			}
			for _, label := range labels {
				f.svc.state[label] = platform.Status{Installed: true, Loaded: true}
			}
			if code, _, stderr := f.run(t, tc.command); code != 0 {
				t.Fatalf("%s: exit %d, stderr %q", tc.command, code, stderr)
			}
			if len(f.svc.waits) != len(labels) {
				t.Fatalf("waits = %v; want one per agent of %v", f.svc.waits, labels)
			}
			for _, wait := range f.svc.waits {
				if wait <= exitTimeOut {
					t.Fatalf("waits = %v; want each bounded and over %s", f.svc.waits, exitTimeOut)
				}
			}
		})
	}
}

func TestInstallRefusals(t *testing.T) {
	f := newFixture(t)
	code, stdout, stderr := f.run(t, "install", "extra")
	expect(t, code, stdout, stderr, 2, "", "usage: localrouter install -home DIR\n")
	f.slotMode(t, "blue\n")
	code, stdout, stderr = f.run(t, "install")
	expect(t, code, stdout, stderr, 1, "", "cutover already installed; use 'router deploy'\n")
	expectCalls(t, f)
	if len(f.killed) != 0 {
		t.Fatalf("a refused install killed %q", f.killed)
	}
}

func TestInstallThatNeverComesUpShowsTheLog(t *testing.T) {
	f := newFixture(t)
	f.upOnLoad = false
	f.write(t, "router.log", "one\ntwo\nthree\nfour\nfive\nsix\n")
	code, stdout, stderr := f.run(t, "install")
	expect(t, code, stdout, stderr, 1, "", "failed to come up; last log lines:\ntwo\nthree\nfour\nfive\nsix\n")
	if f.probes != 25 {
		t.Fatalf("probed %d times; want 25", f.probes)
	}
}

func TestUninstall(t *testing.T) {
	f := newFixture(t)
	f.svc.state[legacyLabel] = platform.Status{Installed: true, Loaded: true}
	code, stdout, stderr := f.run(t, "uninstall")
	expect(t, code, stdout, stderr, 0, "uninstalled com.claude-local-router\n", "")
	expectCalls(t, f, "Uninstall "+legacyLabel)

	f = newFixture(t)
	f.slotMode(t, "blue\n")
	code, stdout, stderr = f.run(t, "uninstall")
	expect(t, code, stdout, stderr, 1, "", "slot-based uninstall requires a maintenance window\n")
	expectCalls(t, f)
}

func TestStartManaged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		loaded bool
		calls  []string
	}{
		{"not loaded", false, []string{"Status " + legacyLabel, "Start " + legacyLabel}},
		{"loaded", true, []string{"Status " + legacyLabel}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.svc.state[legacyLabel] = platform.Status{Installed: true, Loaded: tc.loaded}
			f.alive = tc.loaded
			code, stdout, stderr := f.run(t, "start")
			expect(t, code, stdout, stderr, 0, "started by launchd on 127.0.0.1:8787\n", "")
			expectCalls(t, f, tc.calls...)
		})
	}
}

func TestStartByHand(t *testing.T) {
	f := newFixture(t)
	code, stdout, stderr := f.run(t, "start")
	expect(t, code, stdout, stderr, 0, "started on 127.0.0.1:8787 (pid 4242)\n", "")
	if want := []string{fmt.Sprint(filepath.Join(f.home, "localrouter"), []string(nil), filepath.Join(f.home, "router.log"))}; !reflect.DeepEqual(f.spawned, want) {
		t.Fatalf("spawned %q; want %q", f.spawned, want)
	}
	if pid, err := os.ReadFile(filepath.Join(f.home, "router.pid")); err != nil || string(pid) != "4242\n" {
		t.Fatalf("pidfile = %q, %v", pid, err)
	}

	f = newFixture(t)
	f.alive = true
	code, stdout, stderr = f.run(t, "start")
	expect(t, code, stdout, stderr, 0, "already running on 127.0.0.1:8787\n", "")
	if len(f.spawned) != 0 {
		t.Fatalf("started a second router: %q", f.spawned)
	}
}

func TestStop(t *testing.T) {
	f := newFixture(t)
	f.svc.state[legacyLabel] = platform.Status{Installed: true, Loaded: true}
	code, stdout, stderr := f.run(t, "stop")
	expect(t, code, stdout, stderr, 0, "stopped; launchd will start it again at next login or on 'router start'\n", "")
	expectCalls(t, f, "Status "+legacyLabel, "Stop "+legacyLabel)
	if len(f.killed) != 0 {
		t.Fatalf("stopping the agent also killed %q", f.killed)
	}

	f = newFixture(t)
	f.write(t, "router.pid", "77\n")
	code, stdout, stderr = f.run(t, "stop")
	expect(t, code, stdout, stderr, 0, "stopped\n", "")
	if !reflect.DeepEqual(f.stopped, []int{77}) || len(f.killed) != 1 {
		t.Fatalf("terminated %v, killed %q", f.stopped, f.killed)
	}
}

func TestStatus(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state *platform.Status
		alive bool
		want  string
	}{
		{"not installed", nil, false, "launchd agent: not installed (run 'router install')\nnot running\n"},
		{"loaded", &platform.Status{Installed: true, Loaded: true}, true, "launchd agent com.claude-local-router: loaded\nrunning on 127.0.0.1:8787\n"},
		{"not loaded", &platform.Status{Installed: true}, false, "launchd agent com.claude-local-router: installed but not loaded\nnot running\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.state != nil {
				f.svc.state[legacyLabel] = *tc.state
			}
			f.alive = tc.alive
			code, stdout, stderr := f.run(t, "status")
			expect(t, code, stdout, stderr, 0, tc.want, "")
		})
	}
}

// The env file in the home names the address, as it did when the shell
// script sourced it.
func TestListenAddressComesFromTheEnvFile(t *testing.T) {
	f := newFixture(t)
	f.write(t, "env", "ROUTER_LISTEN=127.0.0.1:9797\n")
	code, stdout, stderr := f.run(t, "status")
	expect(t, code, stdout, stderr, 0, "launchd agent: not installed (run 'router install')\nnot running\n", "")
	if f.health[0] != "http://127.0.0.1:9797/healthz" {
		t.Fatalf("probed %q", f.health)
	}
}

func TestEnvPointsClaudeAtTheRouter(t *testing.T) {
	f := newFixture(t)
	code, stdout, stderr := f.run(t, "env")
	expect(t, code, stdout, stderr, 0, "ANTHROPIC_BASE_URL=http://127.0.0.1:8787\n", "")
	if f.claude != "http://127.0.0.1:8787" {
		t.Fatalf("Claude settings got %q", f.claude)
	}
	expectCalls(t, f)
}

func TestSlotStart(t *testing.T) {
	both := platform.Status{Installed: true}
	for _, tc := range []struct {
		name       string
		marker     string
		state      map[string]platform.Status
		wantCode   int
		wantStdout string
		wantStderr string
		wantStarts []string
	}{
		{"both unloaded", "green\n", map[string]platform.Status{"p.green": both, "p.caddy": both}, 0, "Caddy and green started on 127.0.0.1:8787\n", "", []string{"Start p.green", "Start p.caddy"}},
		{"Caddy unloaded", "green\n", map[string]platform.Status{"p.green": {Installed: true, Loaded: true}, "p.caddy": both}, 0, "Caddy and green started on 127.0.0.1:8787\n", "", []string{"Start p.caddy"}},
		{"bad marker", "purple\n", map[string]platform.Status{"p.green": both, "p.caddy": both}, 1, "", "invalid active slot marker\n", nil},
		{"no marker", "", map[string]platform.Status{"p.green": both, "p.caddy": both}, 1, "", "invalid active slot marker\n", nil},
		{"no Caddy plist", "green\n", map[string]platform.Status{"p.green": both}, 1, "", "slot or Caddy launchd agent missing\n", nil},
		{"no slot plist", "blue\n", map[string]platform.Status{"p.green": both, "p.caddy": both}, 1, "", "slot or Caddy launchd agent missing\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.slotMode(t, tc.marker)
			f.svc.state = tc.state
			code, stdout, stderr := f.run(t, "start")
			expect(t, code, stdout, stderr, tc.wantCode, tc.wantStdout, tc.wantStderr)
			var starts []string
			for _, call := range f.svc.calls {
				if strings.HasPrefix(call, "Start ") {
					starts = append(starts, call)
				}
			}
			if !reflect.DeepEqual(starts, tc.wantStarts) {
				t.Fatalf("started %q; want %q", starts, tc.wantStarts)
			}
		})
	}
}

// Stop is an outage, not a deploy: Caddy goes first, then every loaded slot.
// It needs no marker, so a broken one cannot keep the router up.
func TestSlotStop(t *testing.T) {
	f := newFixture(t)
	f.slotMode(t, "purple\n")
	f.svc.state = map[string]platform.Status{"p.caddy": {Installed: true, Loaded: true}, "p.blue": {Installed: true}, "p.green": {Installed: true, Loaded: true}}
	code, stdout, stderr := f.run(t, "stop")
	expect(t, code, stdout, stderr, 0, "Caddy and both router slots stopped\n", "")
	expectCalls(t, f, "Status p.caddy", "Stop p.caddy", "Status p.blue", "Status p.green", "Stop p.green")
}

func TestSlotStatus(t *testing.T) {
	f := newFixture(t)
	f.slotMode(t, "green\n")
	f.svc.state = map[string]platform.Status{"p.caddy": {Installed: true, Loaded: true}, "p.blue": {Installed: true}, "p.green": {Installed: true, Loaded: true}}
	f.alive = true
	code, stdout, stderr := f.run(t, "status")
	expect(t, code, stdout, stderr, 0, "slot-based router: green\nCaddy: loaded\nblue: not loaded\ngreen: loaded\nrunning on 127.0.0.1:8787\n", "")

	// A broken marker is what status is run to find.
	f = newFixture(t)
	f.slotMode(t, "")
	code, stdout, stderr = f.run(t, "status")
	expect(t, code, stdout, stderr, 0, "slot-based router: unknown\nCaddy: not loaded\nblue: not loaded\ngreen: not loaded\nnot running\n", "")
}

func TestSlotLabelsFailure(t *testing.T) {
	f := newFixture(t)
	f.slotMode(t, "green\n")
	f.labels = errors.New("deploy.json: invalid launchd label prefix \"x.blue\"")
	code, stdout, stderr := f.run(t, "status")
	expect(t, code, stdout, stderr, 1, "", "deploy.json: invalid launchd label prefix \"x.blue\"\n")
}

// A label missing from service-labels would name no agent: stop would
// report Caddy stopped without touching it. The script failed there, and so
// do the commands, before they touch any agent.
func TestSlotLabelMissing(t *testing.T) {
	for _, command := range []string{"start", "stop", "status"} {
		t.Run(command, func(t *testing.T) {
			f := newFixture(t)
			f.slotMode(t, "blue\n")
			f.labelOut = `{"blue":"p.blue","default_prefix":false,"green":"p.green","prefix":"p"}`
			code, stdout, stderr := f.run(t, command)
			expect(t, code, stdout, stderr, 1, "", "service-labels: want blue, green and caddy labels, got "+f.labelOut+"\n")
			expectCalls(t, f)
		})
	}
}
