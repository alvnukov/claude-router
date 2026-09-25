package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"localrouter/internal/platform"
)

// Cutover is the one-time move from the legacy router, which listens on the
// public ports itself, to Caddy in front of the blue/green slots. It is the
// only step that drops connections: between the legacy router's exit and
// Caddy's bind nothing listens on the public ports.

type cutoverOps interface {
	deployOps
	// prepare writes and checks Caddy's config for slot without starting
	// Caddy.
	prepare(context.Context, string) error
	// mark names the slot that may start active; any other name keeps both
	// slots standby.
	mark(context.Context, string) error
	legacyPending(context.Context) (int, error)
	stopLegacy(context.Context) error
	startLegacy(context.Context) error
	startCaddy(context.Context) error
	stopCaddy(context.Context) error
	// commit records the slot configuration for deploy and keeps the legacy
	// agent from starting at the next login.
	commit(context.Context, deployFile) error
}

type cutoverOpsFactory func(file deployFile, home, agents, binary, caddy string) cutoverOps

func newCutoverOps(file deployFile, home, agents, binary, caddy string) cutoverOps {
	return newSystemCutoverOps(file, home, agents, binary, caddy)
}

// runCutover is `localrouter cutover`, run by `./router install --cutover`.
// Every address comes from flags; the router script supplies them.
func runCutover(ctx context.Context, args []string, in io.Reader, out io.Writer, newOps cutoverOpsFactory) error {
	home, _ := os.UserHomeDir()
	flags := flag.NewFlagSet("cutover", flag.ContinueOnError)
	flags.SetOutput(out)
	dir := flags.String("home", env("ROUTER_HOME", filepath.Join(home, ".claude/local-router")), "router home with slot binaries and state")
	agents := flags.String("agents", filepath.Join(home, "Library/LaunchAgents"), "launchd agents directory")
	binary := flags.String("binary", "", "router binary for the blue slot")
	caddy := flags.String("caddy", "", "absolute path of the caddy binary")
	var file deployFile
	flags.StringVar(&file.CaddyAdmin, "caddy-admin", "", "loopback host:port for Caddy's admin API")
	flags.StringVar(&file.PublicAPI, "public-api", "", "API address the legacy router serves now")
	flags.StringVar(&file.PublicUI, "public-ui", "", "UI address the legacy router serves now")
	flags.StringVar(&file.BlueAPI, "blue-api", "", "blue slot API address")
	flags.StringVar(&file.BlueUI, "blue-ui", "", "blue slot UI address")
	flags.StringVar(&file.GreenAPI, "green-api", "", "green slot API address")
	flags.StringVar(&file.GreenUI, "green-ui", "", "green slot UI address")
	flags.StringVar(&file.LabelPrefix, "label-prefix", "", "launchd label prefix recorded in deploy.json; empty keeps the live names")
	wait := flags.Duration("wait", 15*time.Minute, "how long the legacy router may take to finish its requests")
	readyTimeout := flags.Duration("ready-timeout", 30*time.Second, "how long blue, Caddy or the returning legacy router may take to answer")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *binary == "" {
		return errors.New("cutover: -binary is required")
	}
	if _, err := os.Stat(filepath.Join(*dir, "deploy.json")); err == nil {
		return fmt.Errorf("cutover: already done (%s exists); use ./router deploy", filepath.Join(*dir, "deploy.json"))
	}
	if !filepath.IsAbs(*caddy) {
		return fmt.Errorf("cutover: -caddy must be an absolute path, got %q", *caddy)
	}
	file.Caddy = *caddy
	if err := file.validate(); err != nil {
		return fmt.Errorf("cutover: %w", err)
	}
	digest, err := fileDigest(*binary)
	if err != nil {
		return err
	}
	ops := newOps(file, *dir, *agents, *binary, *caddy)
	return withDeployLock(ctx, *dir, func() error {
		return cutover(ctx, file, ops, digest, in, out, *wait, *readyTimeout)
	})
}

func cutover(ctx context.Context, file deployFile, ops cutoverOps, digest string, in io.Reader, out io.Writer, wait, readyTimeout time.Duration) (err error) {
	d := &deployController{config: file.deployConfig, ops: ops}
	switch slot := servingSlot(ctx, file.PublicAPI); slot {
	case "":
	case "blue":
		// An earlier cutover switched but did not record it.
		return recordCutover(ctx, d, file, ops, out)
	default:
		return fmt.Errorf("cutover: slot %s already serves %s; use ./router deploy", slot, file.PublicAPI)
	}
	if conn, err := net.DialTimeout("tcp", file.CaddyAdmin, time.Second); err == nil {
		conn.Close()
		return fmt.Errorf("cutover: something already listens on the Caddy admin address %s", file.CaddyAdmin)
	}
	if _, err := ops.legacyPending(ctx); err != nil {
		return fmt.Errorf("cutover: no legacy router answers on %s: %w", file.PublicUI, err)
	}
	// Until the switch the marker names no slot, so blue, and any launchd
	// restart of it, comes up standby and writes nothing.
	if err := ops.mark(ctx, "legacy"); err != nil {
		return err
	}
	if err := d.retire(ctx, "blue"); err != nil { // left by an earlier attempt
		return err
	}
	legacyStopped, caddyStarted, serving := false, false, false
	defer func() {
		if err == nil || serving {
			return
		}
		// The caller's context may be what failed; getting the legacy router
		// back must not depend on it.
		rollback, cancel := context.WithTimeout(context.WithoutCancel(ctx), readyTimeout+time.Minute)
		defer cancel()
		if legacyStopped {
			err = errors.Join(err, returnToLegacy(rollback, ops, caddyStarted, readyTimeout))
		} else if stop := ops.stop(rollback, "blue"); stop != nil {
			err = errors.Join(err, fmt.Errorf("stop blue: %w", stop))
		}
	}()
	if err = ops.start(ctx, "blue", digest); err != nil {
		return err
	}
	blue, err := waitSlot(ctx, ops, "blue", modeStandby, readyTimeout)
	if err != nil {
		return err
	}
	if err = ops.prepare(ctx, "blue"); err != nil {
		return err
	}
	fmt.Fprintf(out, `Cutover moves %s and %s from the legacy router to Caddy:
  1. blue (pid %d) is up in standby on %s and %s
  2. wait up to %s for the legacy router to finish its requests
  3. stop the legacy router (%s); both ports refuse connections until Caddy binds them
  4. activate blue and start Caddy (%s)
  5. if Caddy does not answer within %s, stop it and blue and start the legacy router again
Type yes to continue: `, file.PublicAPI, file.PublicUI, blue.PID, file.BlueAPI, file.BlueUI, wait, effectiveLabel(file.LabelPrefix, ""), effectiveLabel(file.LabelPrefix, "caddy"), readyTimeout)
	answer, _ := bufio.NewReader(in).ReadString('\n')
	if strings.TrimSpace(answer) != "yes" {
		return errors.New("cutover declined; the legacy router keeps serving")
	}
	if err = waitLegacyIdle(ctx, ops, wait, out); err != nil {
		return err
	}
	err = ops.stopLegacy(ctx)
	legacyStopped = true
	if err != nil {
		return err
	}
	if err = poll(ctx, readyTimeout, 100*time.Millisecond, func() error { return portsFree(file.PublicAPI, file.PublicUI) }); err != nil {
		return err
	}
	if err = ops.save(ctx, "blue"); err != nil {
		return err
	}
	if err = ops.admin(ctx, "blue", "activate"); err != nil {
		return err
	}
	if active, stateErr := ops.state(ctx, "blue"); stateErr != nil || active.Mode != modeActive || active.PID != blue.PID {
		return fmt.Errorf("blue did not activate: %v", stateErr)
	}
	caddyStarted = true
	if err = ops.startCaddy(ctx); err != nil {
		return err
	}
	if err = poll(ctx, readyTimeout, 200*time.Millisecond, func() error { return d.ready(ctx, "blue", blue.PID) }); err != nil {
		return fmt.Errorf("Caddy did not come up: %w", err)
	}
	serving = true
	return recordCutover(ctx, d, file, ops, out)
}

// recordCutover records a cutover once Caddy serves blue: deploy.json, the
// legacy plist moved aside, the history compacted. Run again, it finishes a
// cutover that failed here.
func recordCutover(ctx context.Context, d *deployController, file deployFile, ops cutoverOps, out io.Writer) error {
	blue, err := ops.state(ctx, "blue")
	if err != nil {
		return fmt.Errorf("cutover: blue serves %s but does not answer: %w", file.PublicAPI, err)
	}
	if err := d.ready(ctx, "blue", blue.PID); err != nil {
		return fmt.Errorf("cutover: %w", err)
	}
	if err := ops.commit(ctx, file); err != nil {
		return fmt.Errorf("Caddy and blue serve, but the cutover was not recorded; run it again to record it: %w", err)
	}
	// The legacy router has exited, so nothing else appends to the history.
	if err := ops.admin(ctx, "blue", "compact"); err != nil {
		return fmt.Errorf("blue serves, but its history was not compacted: %w", err)
	}
	fmt.Fprintf(out, "Caddy serves %s and %s from blue (pid %d); ./router deploy switches slots from now on\n", file.PublicAPI, file.PublicUI, blue.PID)
	return nil
}

// returnToLegacy undoes a cutover whose Caddy never answered. Blue stops
// before the legacy router starts, so the two never write at once.
func returnToLegacy(ctx context.Context, ops cutoverOps, caddyStarted bool, readyTimeout time.Duration) error {
	var errs []error
	if caddyStarted {
		errs = append(errs, ops.stopCaddy(ctx))
	}
	errs = append(errs, ops.mark(ctx, "legacy"), ops.stop(ctx, "blue"), ops.startLegacy(ctx))
	errs = append(errs, poll(ctx, readyTimeout, 200*time.Millisecond, func() error {
		_, err := ops.legacyPending(ctx)
		return err
	}))
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("return to the legacy router: %w", err)
	}
	return nil
}

func waitLegacyIdle(ctx context.Context, ops cutoverOps, wait time.Duration, out io.Writer) error {
	last := -1
	err := poll(ctx, wait, 250*time.Millisecond, func() error {
		pending, err := ops.legacyPending(ctx)
		if err != nil {
			return err
		}
		if pending == 0 {
			return nil
		}
		if pending != last {
			fmt.Fprintf(out, "legacy router: %d requests in flight\n", pending)
			last = pending
		}
		return fmt.Errorf("the legacy router still has %d requests in flight after %s; nothing was stopped", pending, wait)
	})
	return err
}

func waitSlot(ctx context.Context, ops deployOps, slot string, mode lifecycleMode, limit time.Duration) (deploySlotState, error) {
	var state deploySlotState
	err := poll(ctx, limit, 100*time.Millisecond, func() error {
		var err error
		state, err = ops.state(ctx, slot)
		if err == nil && (state.Mode != mode || state.PID == 0) {
			err = fmt.Errorf("%s is %s (pid %d), want %s", slot, state.Mode, state.PID, mode)
		}
		return err
	})
	return state, err
}

// poll retries try until it succeeds or limit passes, and returns its last
// error.
func poll(ctx context.Context, limit, every time.Duration, try func() error) error {
	deadline := time.Now().Add(limit)
	for {
		err := try()
		if err == nil || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}

func portsFree(addresses ...string) error {
	for _, address := range addresses {
		if conn, err := net.DialTimeout("tcp", address, time.Second); err == nil {
			conn.Close()
			return fmt.Errorf("%s still accepts connections", address)
		}
	}
	return nil
}

// servingSlot names the slot behind a public address, or "" when the legacy
// router, which answers /healthz with plain text, or nothing listens there.
func servingSlot(ctx context.Context, address string) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/healthz", nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var health struct {
		Slot string `json:"slot"`
	}
	if json.NewDecoder(resp.Body).Decode(&health) != nil {
		return ""
	}
	return health.Slot
}

type systemCutoverOps struct {
	*systemDeployOps
}

func newSystemCutoverOps(file deployFile, home, agents, binary, caddy string) *systemCutoverOps {
	ops := newSystemDeployOps(file.deployConfig, "http://"+file.CaddyAdmin, home, agents, binary)
	ops.caddy = caddy
	ops.labelPrefix = file.LabelPrefix
	return &systemCutoverOps{ops}
}

// caddySpec keeps Caddy's data and config under the router home, away from
// any Caddy the user runs.
func (o *systemCutoverOps) caddySpec() platform.ServiceSpec {
	return platform.ServiceSpec{Label: o.label("caddy"), Exe: o.caddy, Args: []string{"run", "--config", filepath.Join(o.home, "Caddyfile"), "--adapter", "caddyfile"}, Dir: o.home, LogPath: filepath.Join(o.home, "caddy.log"), KeepAlive: true, ExitTimeout: 30 * time.Second,
		Env: map[string]string{"XDG_DATA_HOME": filepath.Join(o.home, "caddy", "data"), "XDG_CONFIG_HOME": filepath.Join(o.home, "caddy", "config")}}
}

// prepare checks the Caddy config with the configured caddy while the legacy
// router still serves. Caddy's agent is installed only when Caddy starts:
// installed earlier, it would take the public ports from the legacy router at
// the next login.
func (o *systemCutoverOps) prepare(ctx context.Context, slot string) error {
	file := filepath.Join(o.home, "Caddyfile")
	if err := platform.WriteFileAtomic(file, []byte(o.caddyfile(slot)), 0o600); err != nil {
		return err
	}
	_, err := o.adapt(ctx, file)
	return err
}

func (o *systemCutoverOps) mark(_ context.Context, slot string) error {
	return platform.WriteFileAtomic(o.markerPath(), []byte(slot+"\n"), 0o600)
}

var (
	legacyBrand       = regexp.MustCompile(`<div class="brand">local-router\b`)
	legacyPendingStat = regexp.MustCompile(`<div class="stat pending">● (\d+) в работе</div>`)
)

// legacyPending reads the in-flight count from the legacy UI's status strip,
// its only report of it. The strip shows the count only when it is not zero.
func (o *systemCutoverOps) legacyPending(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+o.config.PublicUI+"/status", nil)
	if err != nil {
		return 0, err
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	page, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK || !legacyBrand.Match(page) {
		return 0, fmt.Errorf("%s/status is not the router's status page (HTTP %d)", o.config.PublicUI, resp.StatusCode)
	}
	match := legacyPendingStat.FindSubmatch(page)
	if match == nil {
		return 0, nil
	}
	return strconv.Atoi(string(match[1]))
}

func (o *systemCutoverOps) stopLegacy(ctx context.Context) error {
	return o.service.Stop(ctx, o.label(""))
}

func (o *systemCutoverOps) startLegacy(ctx context.Context) error {
	return o.service.Start(ctx, o.label(""))
}

func (o *systemCutoverOps) startCaddy(ctx context.Context) error {
	if err := o.service.Install(ctx, o.caddySpec()); err != nil {
		return err
	}
	return o.service.Start(ctx, o.label("caddy"))
}

// stopCaddy unloads Caddy and takes its plist out of LaunchAgents, so the
// legacy router keeps the ports after the next login too.
func (o *systemCutoverOps) stopCaddy(ctx context.Context) error {
	return o.service.Uninstall(ctx, o.label("caddy"))
}

// commit moves the legacy plist into the router home, where `launchctl
// bootstrap` can still load it by hand, and writes deploy.json last: its
// presence is what marks the cutover done.
func (o *systemCutoverOps) commit(_ context.Context, file deployFile) error {
	if err := os.Rename(filepath.Join(o.agents, o.label("")+".plist"), filepath.Join(o.home, "legacy.plist")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return platform.WriteFileAtomic(filepath.Join(o.home, "deploy.json"), append(data, '\n'), 0o600)
}
