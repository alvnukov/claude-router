package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"localrouter/internal/cli"
	"localrouter/internal/platform"
)

// cutoverFixture puts a fake legacy router on the public ports and lets a
// fake Caddy take them over, over real loopback listeners on test ports.
type cutoverFixture struct {
	*deployFixture
	t         *testing.T
	pending   []int
	marker    string
	committed bool
	file      deployFile
	legacy    []func()
	caddy     []func()
}

func serveOn(t *testing.T, address string, handler http.Handler) func() {
	t.Helper()
	var listener net.Listener
	var err error
	for range 50 { // the previous owner may still be releasing the port
		if listener, err = net.Listen("tcp", address); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }() // ErrServerClosed once the test ends
	return func() { server.Close() }
}

func newCutoverFixture(t *testing.T, pending ...int) *cutoverFixture {
	f := &cutoverFixture{deployFixture: newDeployFixture(t), t: t, pending: pending, marker: "blue"}
	f.api.Close()
	delete(f.slots, "blue")
	f.slots["blue"] = &deploySlotState{Mode: modeStandby}
	f.startLegacyServers()
	t.Cleanup(func() {
		for _, stop := range append(f.legacy, f.caddy...) {
			stop()
		}
	})
	return f
}

func (f *cutoverFixture) startLegacyServers() {
	status := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "legacy") })
	f.legacy = []func(){serveOn(f.t, f.controller.config.PublicAPI, status), serveOn(f.t, f.controller.config.PublicUI, status)}
}

func (f *cutoverFixture) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *cutoverFixture) prepare(_ context.Context, slot string) error {
	f.record("prepare:" + slot)
	return nil
}
func (f *cutoverFixture) mark(_ context.Context, slot string) error {
	f.record("mark:" + slot)
	f.mu.Lock()
	f.marker = slot
	f.mu.Unlock()
	return nil
}
func (f *cutoverFixture) save(ctx context.Context, slot string) error {
	f.record("save:" + slot)
	return f.mark(ctx, slot)
}
func (f *cutoverFixture) legacyPending(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "legacy-pending")
	n := f.pending[0]
	if len(f.pending) > 1 {
		f.pending = f.pending[1:]
	}
	return n, nil
}
func (f *cutoverFixture) stopLegacy(context.Context) error {
	f.record("stop-legacy")
	for _, stop := range f.legacy {
		stop()
	}
	f.legacy = nil
	return nil
}
func (f *cutoverFixture) startLegacy(context.Context) error {
	f.record("start-legacy")
	f.startLegacyServers()
	return nil
}
func (f *cutoverFixture) startCaddy(context.Context) error {
	f.record("start-caddy")
	if f.fail == "caddy" {
		return nil // launchd took the label, but Caddy never answers
	}
	health := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		s := f.slots["blue"]
		fmt.Fprintf(w, `{"slot":"blue","pid":%d,"mode":%q}`, s.PID, s.Mode)
	})
	f.caddy = []func(){serveOn(f.t, f.controller.config.PublicAPI, health), serveOn(f.t, f.controller.config.PublicUI, health)}
	return nil
}
func (f *cutoverFixture) stopCaddy(context.Context) error {
	f.record("stop-caddy")
	for _, stop := range f.caddy {
		stop()
	}
	f.caddy = nil
	return nil
}
func (f *cutoverFixture) commit(_ context.Context, file deployFile) error {
	f.record("commit")
	f.committed = true
	f.file = file
	return nil
}

func (f *cutoverFixture) run(answer string, extra ...string) (string, error) {
	home := f.t.TempDir()
	binary := deployBinary(f.t, home)
	cfg := f.controller.config
	args := []string{"-home", home, "-binary", binary, "-caddy", filepath.Join(home, "nonexistent-caddy"), "-caddy-admin", "127.0.0.1:1",
		"-public-api", cfg.PublicAPI, "-public-ui", cfg.PublicUI, "-blue-api", cfg.BlueAPI, "-blue-ui", cfg.BlueUI,
		"-green-api", cfg.GreenAPI, "-green-ui", cfg.GreenUI, "-wait", "2s", "-ready-timeout", "2s"}
	var out bytes.Buffer
	err := runCutover(f.t.Context(), append(args, extra...), strings.NewReader(answer), &out, func(deployFile, string, string, string, string) cutoverOps { return f })
	return out.String(), err
}

// order checks that the steps happened in this order, each after the last.
func (f *cutoverFixture) order(t *testing.T, steps ...string) {
	t.Helper()
	got := strings.Join(f.calls, ",")
	rest := got
	for _, step := range steps {
		i := strings.Index(rest, step)
		if i < 0 {
			t.Fatalf("%s missing or out of order: %s", step, got)
		}
		rest = rest[i+len(step):]
	}
}

func TestCutoverMovesPublicPortsFromLegacyToCaddy(t *testing.T) {
	f := newCutoverFixture(t, 2, 1, 0)
	out, err := f.run("yes\n")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	f.order(t, "mark:legacy", "start:blue", "legacy-pending", "stop-legacy", "save:blue", "activate:blue", "start-caddy", "commit", "compact:blue")
	if f.slots["blue"].Mode != modeActive || f.marker != "blue" || !f.committed {
		t.Fatalf("blue not serving: %+v marker=%s", f.slots["blue"], f.marker)
	}
	if !strings.Contains(out, "yes") || !strings.Contains(out, "2s") {
		t.Fatalf("plan did not name the confirmation and the wait limit: %s", out)
	}
	resp, err := http.Get("http://" + f.controller.config.PublicAPI + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestCutoverRecordsLabelPrefixInDeployFile(t *testing.T) {
	f := newCutoverFixture(t, 0)
	if out, err := f.run("yes\n", "-label-prefix", "com.claude-local-router.check-1"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if f.file.LabelPrefix != "com.claude-local-router.check-1" {
		t.Fatalf("deploy.json would name other labels: %+v", f.file)
	}
	bad := newCutoverFixture(t, 0)
	if _, err := bad.run("yes\n", "-label-prefix", "com.example/router"); err == nil || len(bad.calls) != 0 {
		t.Fatalf("cutover ran with an invalid label prefix: err=%v calls=%v", err, bad.calls)
	}
}

func TestCutoverWithoutConfirmationLeavesLegacyServing(t *testing.T) {
	f := newCutoverFixture(t, 0)
	if _, err := f.run("no\n"); err == nil {
		t.Fatal("cut over without confirmation")
	}
	f.order(t, "start:blue", "stop:blue")
	if got := strings.Join(f.calls, ","); strings.Contains(got, "stop-legacy") || strings.Contains(got, "legacy-pending,legacy-pending") || f.marker == "blue" || f.committed {
		t.Fatalf("declined cutover touched legacy: %s", got)
	}
}

func TestCutoverGivesUpWhileLegacyIsBusy(t *testing.T) {
	f := newCutoverFixture(t, 0, 1)
	if _, err := f.run("yes\n", "-wait", "300ms"); err == nil {
		t.Fatal("stopped legacy with a request in flight")
	}
	f.order(t, "start:blue", "legacy-pending", "stop:blue")
	if got := strings.Join(f.calls, ","); strings.Contains(got, "stop-legacy") {
		t.Fatalf("busy legacy was stopped: %s", got)
	}
}

func TestCutoverRefusesWhenSlotsAlreadyServe(t *testing.T) {
	f := newCutoverFixture(t, 0)
	if err := f.stopLegacy(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.caddy = []func(){serveOn(t, f.controller.config.PublicAPI, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"slot":"blue","pid":7,"mode":"active"}`)
	}))}
	f.calls = nil
	if _, err := f.run("yes\n"); err == nil {
		t.Fatal("cut over a router that already runs on slots")
	}
	if got := strings.Join(f.calls, ","); strings.Contains(got, "stop") || strings.Contains(got, "start") || strings.Contains(got, "mark") {
		t.Fatalf("serving slots were touched: %s", got)
	}
}

// A cutover whose last step failed leaves Caddy serving blue with nothing
// recorded; deploy then sends the user back to cutover, which finishes it.
func TestCutoverRecordsASwitchItFailedToRecord(t *testing.T) {
	f := newCutoverFixture(t, 0)
	if err := f.stopLegacy(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.slots["blue"] = &deploySlotState{Mode: modeActive, PID: 7}
	if err := f.startCaddy(t.Context()); err != nil {
		t.Fatal(err)
	}
	admin := httptest.NewServer(http.NotFoundHandler()) // Caddy's own admin API
	defer admin.Close()
	f.calls = nil
	if out, err := f.run("", "-caddy-admin", admin.Listener.Addr().String()); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := strings.Join(f.calls, ","); got != "state:blue,commit,compact:blue" || !f.committed {
		t.Fatalf("finishing the cutover did more than record it: %s", got)
	}
}

func TestCutoverRefusesBusyCaddyAdmin(t *testing.T) {
	f := newCutoverFixture(t, 0)
	admin := httptest.NewServer(http.NotFoundHandler())
	defer admin.Close()
	if _, err := f.run("yes\n", "-caddy-admin", admin.Listener.Addr().String()); err == nil || len(f.calls) != 0 {
		t.Fatalf("started with another process on the Caddy admin port: err=%v calls=%v", err, f.calls)
	}
}

func TestCutoverReturnsToLegacyWhenCaddyDoesNotAnswer(t *testing.T) {
	f := newCutoverFixture(t, 0)
	f.fail = "caddy"
	var left time.Duration
	f.onStop = func(ctx context.Context, slot string) {
		if deadline, ok := ctx.Deadline(); ok && slot == "blue" {
			left = time.Until(deadline)
		}
	}
	if _, err := f.run("yes\n", "-ready-timeout", "300ms"); err == nil {
		t.Fatal("cutover succeeded without Caddy")
	}
	f.order(t, "stop-legacy", "start-caddy", "stop-caddy", "mark:legacy", "stop:blue", "start-legacy")
	// Caddy stopped at once here; launchd may take its time with Caddy and
	// blue alike, and the legacy router must still come back.
	if want := 2*cli.StopTimeout + 300*time.Millisecond - time.Second; left < want {
		t.Fatalf("the rollback had %s left when it stopped blue; want %s", left, want)
	}
	if f.committed || f.marker != "legacy" {
		t.Fatalf("rollback left slot mode behind: marker=%s committed=%v", f.marker, f.committed)
	}
	resp, err := http.Get("http://" + f.controller.config.PublicUI + "/status")
	if err != nil {
		t.Fatalf("legacy not back on its port: %v", err)
	}
	resp.Body.Close()
}

func TestCutoverRefusesSecondRun(t *testing.T) {
	f := newCutoverFixture(t, 0)
	home := t.TempDir()
	writeDeployFile(t, home, f.controller.config)
	err := runCutover(t.Context(), []string{"-home", home, "-binary", deployBinary(t, home)}, strings.NewReader("yes\n"), io.Discard, func(deployFile, string, string, string, string) cutoverOps { return f })
	if err == nil || len(f.calls) != 0 {
		t.Fatalf("second cutover ran: err=%v calls=%v", err, f.calls)
	}
}

func TestLegacyPendingReadsStatusStrip(t *testing.T) {
	pages := map[string]string{
		"/busy":  `<div class="strip"><div class="brand">local-router <span class="muted">1m</span></div><div class="stat pending">● 3 в работе</div></div>`,
		"/idle":  `<div class="strip"><div class="brand">local-router <span class="muted">1m</span></div><div class="stat"><b>0</b></div></div>`,
		"/other": `<html>something else</html>`,
	}
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, pages[path])
	}))
	defer server.Close()
	cfg := testDeployConfig(t)
	cfg.PublicUI = server.Listener.Addr().String()
	home := t.TempDir()
	ops := newSystemCutoverOps(deployFile{CaddyAdmin: "127.0.0.1:1", deployConfig: cfg}, home, filepath.Join(home, "agents"), filepath.Join(home, "binary"), "/opt/caddy")
	for p, want := range map[string]int{"/busy": 3, "/idle": 0} {
		path = p
		if got, err := ops.legacyPending(t.Context()); err != nil || got != want {
			t.Fatalf("%s: pending %d, want %d: %v", p, got, want, err)
		}
	}
	path = "/other"
	if _, err := ops.legacyPending(t.Context()); err == nil {
		t.Fatal("read pending from a page that is not the router's")
	}
}

func TestSystemCutoverUsesConfiguredScratchLabels(t *testing.T) {
	cfg := testDeployConfig(t)
	home := t.TempDir()
	prefix := "com.claude-local-router.scratch-123"
	ops := newSystemCutoverOps(deployFile{CaddyAdmin: "127.0.0.1:1", LabelPrefix: prefix, deployConfig: cfg}, home, filepath.Join(home, "agents"), filepath.Join(home, "binary"), "/opt/caddy")
	if got := ops.caddySpec().Label; got != prefix+".caddy" {
		t.Fatalf("Caddy label %q", got)
	}
	calls := recordLaunchctl(ops.systemDeployOps)
	if err := ops.stopLegacy(t.Context()); err != nil || len(*calls) != 2 || strings.Count(strings.Join(*calls, "\n"), prefix) != 2 {
		t.Fatalf("stopLegacy touched another label: %v %v", err, *calls)
	}
}

// Stop waits for launchd to unload the agent, and nothing else ends a stop
// launchd never finishes: each one gives up once launchd has had the longest
// ExitTimeOut it grants, 60 s, and time to kill and unload.
func TestSystemStopsGiveUpAfterLaunchdHadItsTime(t *testing.T) {
	home := t.TempDir()
	ops := newSystemCutoverOps(deployFile{CaddyAdmin: "127.0.0.1:1", deployConfig: testDeployConfig(t)}, home, filepath.Join(home, "agents"), filepath.Join(home, "binary"), "/opt/caddy")
	var left []time.Duration
	ops.service = platform.Launchd{Dir: ops.agents, Domain: platform.LaunchdDomain(), Run: func(ctx context.Context, args ...string) error {
		if deadline, ok := ctx.Deadline(); ok && args[0] == "bootout" {
			left = append(left, time.Until(deadline))
		}
		if args[0] == "print" {
			return platform.ErrNotLoaded
		}
		return nil
	}}
	for _, stop := range []func(context.Context) error{func(ctx context.Context) error { return ops.stop(ctx, "blue") }, ops.stopLegacy, ops.stopCaddy} {
		if err := stop(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(left) != 3 {
		t.Fatalf("stops without a deadline: %d of 3 bounded", len(left))
	}
	for _, wait := range left {
		if wait <= time.Minute || wait > cli.StopTimeout {
			t.Fatalf("stops wait %v; want past launchd's 60 s and at most %s", left, cli.StopTimeout)
		}
	}
}

func TestSystemCutoverDrivesLaunchdLabelsAndMovesLegacyPlist(t *testing.T) {
	skipDarwinOnlyDeploy(t)
	cfg := testDeployConfig(t)
	home := t.TempDir()
	agents := filepath.Join(home, "agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(agents, "com.claude-local-router.plist")
	if err := os.WriteFile(legacy, []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A stand-in caddy whose adapt succeeds; the real one is exercised in
	// deploy_caddy_test.go.
	caddy := filepath.Join(home, "caddy")
	if err := os.WriteFile(caddy, []byte("#!/bin/sh\necho '{}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := deployFile{CaddyAdmin: "127.0.0.1:1", Caddy: caddy, deployConfig: cfg}
	ops := newSystemCutoverOps(file, home, agents, filepath.Join(home, "binary"), caddy)
	commands := recordLaunchctl(ops.systemDeployOps)
	if err := ops.prepare(t.Context(), "blue"); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(agents, "com.claude-local-router.caddy.plist")
	if _, err := os.Stat(installed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Caddy plist in LaunchAgents before Caddy starts would take the ports at next login: %v", err)
	}
	if caddyfile, err := os.ReadFile(filepath.Join(home, "Caddyfile")); err != nil || !strings.Contains(string(caddyfile), cfg.BlueAPI) {
		t.Fatalf("prepare did not write the blue Caddyfile: %v", err)
	}
	if marker, err := os.ReadFile(filepath.Join(home, "active-slot")); err == nil {
		t.Fatalf("prepare named an active slot: %s", marker)
	}
	for _, step := range []func(context.Context) error{ops.stopLegacy, ops.startLegacy, ops.startCaddy} {
		if err := step(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	caddyPlist, err := os.ReadFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{caddy, "<string>run</string>", filepath.Join(home, "Caddyfile"), "KeepAlive", "XDG_DATA_HOME", "XDG_CONFIG_HOME", filepath.Join(home, "caddy.log")} {
		if !strings.Contains(string(caddyPlist), want) {
			t.Fatalf("Caddy plist lacks %s: %s", want, caddyPlist)
		}
	}
	if err := ops.stopCaddy(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(installed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stopped Caddy would come back at next login: %v", err)
	}
	domain := platform.LaunchdDomain()
	want := []string{"bootout " + domain + "/com.claude-local-router", "print " + domain + "/com.claude-local-router", "bootstrap " + domain + " " + legacy,
		"bootstrap " + domain + " " + installed, "bootout " + domain + "/com.claude-local-router.caddy", "print " + domain + "/com.claude-local-router.caddy"}
	if strings.Join(*commands, "\n") != strings.Join(want, "\n") {
		t.Fatalf("launchctl calls:\n%s\nwant:\n%s", strings.Join(*commands, "\n"), strings.Join(want, "\n"))
	}
	if err := ops.commit(t.Context(), file); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy plist would start at next login: %v", err)
	}
	if kept, err := os.ReadFile(filepath.Join(home, "legacy.plist")); err != nil || string(kept) != "legacy" {
		t.Fatalf("legacy plist not kept for a manual return: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, "deploy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved deployFile
	if err := json.Unmarshal(data, &saved); err != nil || saved != file {
		t.Fatalf("deploy.json does not round-trip: %+v %v", saved, err)
	}
}

// The slot and Caddy agents a cutover already installed must stay byte for
// byte what the next deploy writes. The fixed paths and addresses match the
// goldens that platform's renderer test keeps.
func TestSlotAndCaddyPlistsMatchGolden(t *testing.T) {
	skipDarwinOnlyDeploy(t)
	home := "/home/router/.claude/local-router"
	cfg := deployConfig{PublicAPI: "127.0.0.1:18787", PublicUI: "127.0.0.1:18788", BlueAPI: "127.0.0.1:18791", BlueUI: "127.0.0.1:18793", GreenAPI: "127.0.0.1:18792", GreenUI: "127.0.0.1:18794"}
	ops := newSystemCutoverOps(deployFile{CaddyAdmin: "127.0.0.1:12019", deployConfig: cfg}, home, "/home/router/Library/LaunchAgents", home+"/localrouter.candidate", "/opt/homebrew/bin/caddy")
	for golden, got := range map[string][]byte{
		"launchd-com.claude-local-router.green.plist": platform.LaunchdPlist(ops.slotSpec("green")),
		"launchd-com.claude-local-router.caddy.plist": platform.LaunchdPlist(ops.caddySpec()),
	} {
		want, err := os.ReadFile(filepath.Join("internal", "platform", "testdata", golden))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s differs:\n--- got\n%s\n--- want\n%s", golden, got, want)
		}
	}
}
