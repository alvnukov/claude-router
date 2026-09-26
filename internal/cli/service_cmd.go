package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"localrouter/internal/platform"
)

// Router is what the service commands take from the router's package main,
// which owns its names and settings until they move under internal/.
type Router struct {
	Label        string                                   // the agent's label outside slot mode
	SlotLabels   func(args []string, out io.Writer) error // the service-labels command
	ClientURL    func(listen string) (string, error)      // the URL clients reach a listen address by
	SetClaudeURL func(url string) error                   // points Claude Code's settings at url
	ReadEnv      func(path string) (map[string]string, error)
}

// Host is the machine under the service commands.
type Host struct {
	Service      platform.Service
	Healthy      func(ctx context.Context, url string) bool
	Spawn        func(exe string, args []string, logPath string) (int, error)
	Terminate    func(pid int) error
	KillMatching func(pattern string) error
	ProbeEvery   time.Duration // between health probes while the router comes up
}

// SystemHost is this machine. Where the OS has no service manager yet, every
// service call fails with the reason, and env, which needs none, still works.
func SystemHost() Host {
	svc, err := platform.NewService()
	if err != nil {
		svc = unsupported{fmt.Errorf("service management: %w", err)}
	}
	client := &http.Client{Timeout: time.Second}
	return Host{
		Service: svc,
		Healthy: func(ctx context.Context, url string) bool {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return false
			}
			resp, err := client.Do(req)
			if err != nil {
				return false
			}
			resp.Body.Close()
			return resp.StatusCode < 400
		},
		Spawn:        platform.StartDetached,
		Terminate:    platform.Terminate,
		KillMatching: platform.KillMatching,
		ProbeEvery:   200 * time.Millisecond,
	}
}

// Commands are the subcommands that install, run and inspect the router as
// a per-user service. Each takes -home, the directory with the binary, its
// env file and log, and deploy.json once install --cutover has run; with
// deploy.json the router runs as blue and green slots behind Caddy.
func Commands(r Router, h Host) []Entry {
	c := &commands{r: r, h: h}
	return []Entry{
		c.entry("install", c.install),
		c.entry("uninstall", c.uninstall),
		c.entry("start", c.start),
		c.entry("stop", c.stop),
		c.entry("status", c.status),
		c.entry("env", c.env),
	}
}

type commands struct {
	r Router
	h Host
}

// site is a router home as the commands see it.
type site struct {
	home string
	addr string // the address messages name
	url  string // the address clients use
	slot bool   // deploy.json exists
}

func (c *commands) entry(name string, run func(context.Context, site, io.Writer) error) Entry {
	return Entry{Name: name, Run: func(ctx context.Context, args []string, stdout, stderr io.Writer) error {
		usage := UsageError("usage: localrouter " + name + " -home DIR")
		flags := flag.NewFlagSet(name, flag.ContinueOnError)
		flags.SetOutput(stderr)
		home := flags.String("home", "", "router home: the binary, its env file and log, deploy.json")
		if err := flags.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return err
			}
			return usage
		}
		if flags.NArg() > 0 || *home == "" {
			return usage
		}
		s, err := c.open(*home)
		if err != nil {
			return err
		}
		return run(ctx, s, stdout)
	}}
}

// open reads the address from the env file in home, which wins over the
// environment, as it did when the shell script sourced it.
func (c *commands) open(home string) (site, error) {
	vals, err := c.r.ReadEnv(filepath.Join(home, "env"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return site{}, err
	}
	listen := vals["ROUTER_LISTEN"]
	if listen == "" {
		listen = os.Getenv("ROUTER_LISTEN")
	}
	url, err := c.r.ClientURL(listen)
	if err != nil {
		return site{}, err
	}
	addr := listen
	if addr == "" {
		addr = strings.TrimPrefix(url, "http://")
	}
	_, err = os.Stat(filepath.Join(home, "deploy.json"))
	return site{home: home, addr: addr, url: url, slot: err == nil}, nil
}

// exitTimeout is how long launchd lets a router agent drain its requests
// after SIGTERM before it kills it: the drain, 900 s unless
// ROUTER_DRAIN_TIMEOUT says otherwise, and a minute to exit. The slots have
// the same.
const exitTimeout = 960 * time.Second

// stopTimeout bounds the wait for an agent to unload: its exitTimeout and a
// minute for launchd.
const stopTimeout = exitTimeout + time.Minute

// stopAgent unloads the agent and waits for its program to exit, at most
// stopTimeout.
func (c *commands) stopAgent(ctx context.Context, label string) error {
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	return c.h.Service.Stop(ctx, label)
}

// legacySpec is the agent the shell script used to write: the binary in the
// home, restarted whenever it exits, at most every ten seconds.
func legacySpec(label, home string) platform.ServiceSpec {
	return platform.ServiceSpec{
		Label:            label,
		Exe:              filepath.Join(home, "localrouter"),
		Dir:              home,
		LogPath:          filepath.Join(home, "router.log"),
		KeepAlive:        true,
		ThrottleInterval: 10 * time.Second,
	}
}

func (c *commands) install(ctx context.Context, s site, stdout io.Writer) error {
	if s.slot {
		return errors.New("cutover already installed; use 'router deploy'")
	}
	// A hand-started instance would hold the port against the agent.
	if err := c.stopByHand(s); err != nil {
		return err
	}
	svc, label := c.h.Service, c.r.Label
	if err := svc.Install(ctx, legacySpec(label, s.home)); err != nil {
		return err
	}
	st, err := svc.Status(ctx, label)
	if err != nil {
		return err
	}
	// Unloading and loading again restarts the router on the new definition.
	if st.Loaded {
		if err := c.stopAgent(ctx, label); err != nil {
			return err
		}
	}
	if err := svc.Start(ctx, label); err != nil {
		return err
	}
	if err := c.waitUp(ctx, s); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "installed as %s, listening on %s\n", label, s.addr)
	return nil
}

func (c *commands) uninstall(ctx context.Context, s site, stdout io.Writer) error {
	if s.slot {
		return errors.New("slot-based uninstall requires a maintenance window")
	}
	// Uninstall stops the agent first.
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	if err := c.h.Service.Uninstall(ctx, c.r.Label); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "uninstalled %s\n", c.r.Label)
	return nil
}

func (c *commands) start(ctx context.Context, s site, stdout io.Writer) error {
	if s.slot {
		return c.startSlots(ctx, s, stdout)
	}
	st, err := c.h.Service.Status(ctx, c.r.Label)
	if err != nil {
		return err
	}
	if st.Installed {
		if !st.Loaded {
			if err := c.h.Service.Start(ctx, c.r.Label); err != nil {
				return err
			}
		}
		if err := c.waitUp(ctx, s); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "started by launchd on %s\n", s.addr)
		return nil
	}
	if c.h.Healthy(ctx, s.url+"/healthz") {
		fmt.Fprintf(stdout, "already running on %s\n", s.addr)
		return nil
	}
	pid, err := c.h.Spawn(filepath.Join(s.home, "localrouter"), nil, filepath.Join(s.home, "router.log"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.home, "router.pid"), []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return err
	}
	if err := c.waitUp(ctx, s); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "started on %s (pid %d)\n", s.addr, pid)
	return nil
}

func (c *commands) stop(ctx context.Context, s site, stdout io.Writer) error {
	if s.slot {
		return c.stopSlots(ctx, s, stdout)
	}
	st, err := c.h.Service.Status(ctx, c.r.Label)
	if err != nil {
		return err
	}
	if st.Installed && st.Loaded {
		if err := c.stopAgent(ctx, c.r.Label); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "stopped; launchd will start it again at next login or on 'router start'")
		return nil
	}
	if err := c.stopByHand(s); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "stopped")
	return nil
}

func (c *commands) status(ctx context.Context, s site, stdout io.Writer) error {
	if s.slot {
		return c.statusSlots(ctx, s, stdout)
	}
	st, err := c.h.Service.Status(ctx, c.r.Label)
	if err != nil {
		return err
	}
	switch {
	case st.Installed && st.Loaded:
		fmt.Fprintf(stdout, "launchd agent %s: loaded\n", c.r.Label)
	case st.Installed:
		fmt.Fprintf(stdout, "launchd agent %s: installed but not loaded\n", c.r.Label)
	default:
		fmt.Fprintln(stdout, "launchd agent: not installed (run 'router install')")
	}
	c.printRunning(ctx, s, stdout)
	return nil
}

// env points Claude Code at the router.
func (c *commands) env(_ context.Context, s site, stdout io.Writer) error {
	if err := c.r.SetClaudeURL(s.url); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "ANTHROPIC_BASE_URL=%s\n", s.url)
	return nil
}

func (c *commands) printRunning(ctx context.Context, s site, stdout io.Writer) {
	if c.h.Healthy(ctx, s.url+"/healthz") {
		fmt.Fprintf(stdout, "running on %s\n", s.addr)
	} else {
		fmt.Fprintln(stdout, "not running")
	}
}

// stopByHand stops a router started without the service manager.
func (c *commands) stopByHand(s site) error {
	pidfile := filepath.Join(s.home, "router.pid")
	if data, err := os.ReadFile(pidfile); err == nil {
		// A pid of 0 or less would signal a whole process group.
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
			_ = c.h.Terminate(pid) // a stale pidfile names a process long gone
		}
		os.Remove(pidfile)
	}
	// Also catch an instance started by claude-local, which writes no
	// pidfile. Anchored: the slot binaries localrouter.blue and .green share
	// the prefix.
	return c.h.KillMatching(regexp.QuoteMeta(filepath.Join(s.home, "localrouter")) + "( |$)")
}

// waitUp gives the router 25 probes, ProbeEvery apart, to answer; if it
// does not, the error carries the last lines of its log.
func (c *commands) waitUp(ctx context.Context, s site) error {
	for i := range 25 {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.h.ProbeEvery):
			}
		}
		if c.h.Healthy(ctx, s.url+"/healthz") {
			return nil
		}
	}
	msg := "failed to come up; last log lines:"
	if data, err := os.ReadFile(filepath.Join(s.home, "router.log")); err == nil && len(bytes.TrimSpace(data)) > 0 {
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		msg += "\n" + strings.Join(lines[max(0, len(lines)-5):], "\n")
	}
	return errors.New(msg)
}

type slotLabels struct {
	Blue  string `json:"blue"`
	Green string `json:"green"`
	Caddy string `json:"caddy"`
}

// slotLabels asks the service-labels command, which validates deploy.json,
// for the agents' names. A name it leaves out is an error: an empty label
// names no agent, so stop would pass over one that runs.
func (c *commands) slotLabels(home string) (slotLabels, error) {
	var out bytes.Buffer
	if err := c.r.SlotLabels([]string{"-home", home}, &out); err != nil {
		return slotLabels{}, err
	}
	var labels slotLabels
	if err := json.Unmarshal(out.Bytes(), &labels); err != nil {
		return slotLabels{}, fmt.Errorf("service-labels: %w", err)
	}
	if labels.Blue == "" || labels.Green == "" || labels.Caddy == "" {
		return slotLabels{}, fmt.Errorf("service-labels: want blue, green and caddy labels, got %s", bytes.TrimSpace(out.Bytes()))
	}
	return labels, nil
}

// activeSlot reads the marker naming the slot Caddy sends traffic to.
func activeSlot(home string) (string, error) {
	data, err := os.ReadFile(filepath.Join(home, "active-slot"))
	if slot := strings.TrimSpace(string(data)); err == nil && (slot == "blue" || slot == "green") {
		return slot, nil
	}
	return "", errors.New("invalid active slot marker")
}

func (c *commands) startSlots(ctx context.Context, s site, stdout io.Writer) error {
	slot, err := activeSlot(s.home)
	if err != nil {
		return err
	}
	labels, err := c.slotLabels(s.home)
	if err != nil {
		return err
	}
	label := labels.Blue
	if slot == "green" {
		label = labels.Green
	}
	var states []platform.Status
	for _, l := range []string{label, labels.Caddy} {
		st, err := c.h.Service.Status(ctx, l)
		if err != nil {
			return err
		}
		if !st.Installed {
			return errors.New("slot or Caddy launchd agent missing")
		}
		states = append(states, st)
	}
	for i, l := range []string{label, labels.Caddy} {
		if !states[i].Loaded {
			if err := c.h.Service.Start(ctx, l); err != nil {
				return err
			}
		}
	}
	if err := c.waitUp(ctx, s); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Caddy and %s started on %s\n", slot, s.addr)
	return nil
}

// stopSlots needs no marker, so a broken one cannot keep the router up.
func (c *commands) stopSlots(ctx context.Context, s site, stdout io.Writer) error {
	labels, err := c.slotLabels(s.home)
	if err != nil {
		return err
	}
	// Explicit stop is an outage, not a deploy. Caddy stops taking new
	// requests, then both KeepAlive agents are unloaded until next login.
	for _, label := range []string{labels.Caddy, labels.Blue, labels.Green} {
		st, err := c.h.Service.Status(ctx, label)
		if err != nil {
			return err
		}
		if st.Loaded {
			if err := c.stopAgent(ctx, label); err != nil {
				return err
			}
		}
	}
	fmt.Fprintln(stdout, "Caddy and both router slots stopped")
	return nil
}

// statusSlots reports a broken marker instead of failing on it: finding one
// is what status is run for.
func (c *commands) statusSlots(ctx context.Context, s site, stdout io.Writer) error {
	labels, err := c.slotLabels(s.home)
	if err != nil {
		return err
	}
	slot, err := activeSlot(s.home)
	if err != nil {
		slot = "unknown"
	}
	fmt.Fprintf(stdout, "slot-based router: %s\n", slot)
	for _, agent := range []struct{ name, label string }{{"Caddy", labels.Caddy}, {"blue", labels.Blue}, {"green", labels.Green}} {
		st, err := c.h.Service.Status(ctx, agent.label)
		if err != nil {
			return err
		}
		if st.Loaded {
			fmt.Fprintf(stdout, "%s: loaded\n", agent.name)
		} else {
			fmt.Fprintf(stdout, "%s: not loaded\n", agent.name)
		}
	}
	c.printRunning(ctx, s, stdout)
	return nil
}

// unsupported is the Service of a system the router cannot install itself
// on yet.
type unsupported struct{ err error }

func (u unsupported) Install(context.Context, platform.ServiceSpec) error { return u.err }
func (u unsupported) Uninstall(context.Context, string) error             { return u.err }
func (u unsupported) Start(context.Context, string) error                 { return u.err }
func (u unsupported) Stop(context.Context, string) error                  { return u.err }
func (u unsupported) Status(context.Context, string) (platform.Status, error) {
	return platform.Status{}, u.err
}
