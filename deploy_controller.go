package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"
)

type deployConfig struct {
	PublicAPI string `json:"public_api"`
	PublicUI  string `json:"public_ui"`
	BlueAPI   string `json:"blue_api"`
	BlueUI    string `json:"blue_ui"`
	GreenAPI  string `json:"green_api"`
	GreenUI   string `json:"green_ui"`
}

// validate accepts distinct loopback host:port addresses. A test binary may
// never name a live router port, whoever built the config.
func (c deployConfig) validate() error {
	addresses := []string{c.PublicAPI, c.PublicUI, c.BlueAPI, c.BlueUI, c.GreenAPI, c.GreenUI}
	seen := make(map[string]bool)
	for _, address := range addresses {
		host, port, err := net.SplitHostPort(address)
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || port == "0" || seen[address] {
			return fmt.Errorf("invalid or duplicate loopback address %q", address)
		}
		if testing.Testing() {
			switch port {
			case "8787", "8788", "8791", "8792", "8793", "8794":
				return fmt.Errorf("reserved live port %s cannot be used in tests", port)
			}
		}
		seen[address] = true
	}
	return nil
}

type deploySlotState struct {
	Slot    string        `json:"slot"`
	Mode    lifecycleMode `json:"mode"`
	PID     int           `json:"pid"`
	Pending int           `json:"pending"`
	Digest  string        `json:"digest"`
}

type deployOps interface {
	current(context.Context) (string, error)
	state(context.Context, string) (deploySlotState, error)
	start(context.Context, string, string) error
	admin(context.Context, string, string) error
	flip(context.Context, string) error
	stop(context.Context, string) error
	save(context.Context, string) error
}

type deployController struct {
	config       deployConfig
	ops          deployOps
	client       *http.Client
	readyTimeout time.Duration // how long a started slot may take to answer
}

func (d *deployController) ready(ctx context.Context, slot string, pid int) error {
	client := d.client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+d.config.PublicAPI+"/healthz", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("public readiness: HTTP %d", response.StatusCode)
	}
	var state struct {
		Slot string        `json:"slot"`
		PID  int           `json:"pid"`
		Mode lifecycleMode `json:"mode"`
	}
	if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
		return err
	}
	if state.Slot != slot || state.PID != pid || state.Mode != modeActive {
		return fmt.Errorf("public readiness reached %s pid %d mode %s, want %s pid %d active", state.Slot, state.PID, state.Mode, slot, pid)
	}
	return nil
}

func (d *deployController) deploy(ctx context.Context, digest string, force bool) (err error) {
	if err := d.config.validate(); err != nil {
		return err
	}
	old, err := d.ops.current(ctx)
	if err != nil {
		return err
	}
	if old != "blue" && old != "green" {
		return fmt.Errorf("unknown active slot %q", old)
	}
	newSlot := "blue"
	if old == "blue" {
		newSlot = "green"
	}
	current, err := d.ops.state(ctx, old)
	if err != nil || current.Mode != modeActive {
		return fmt.Errorf("old slot is not active: %w", err)
	}
	// Whatever an interrupted deploy left on the other slot goes first: a
	// draining old slot finishes its requests, a stale candidate is unloaded.
	if err = d.retire(ctx, newSlot); err != nil {
		return err
	}
	if current.Digest == digest && !force {
		return d.ops.save(ctx, old)
	}
	started, quiesced, flipped, committed := false, false, false, false
	defer func() {
		if err == nil || !started || committed {
			return
		}
		if flipped {
			if rollback := d.ops.flip(ctx, old); rollback != nil {
				err = errors.Join(err, fmt.Errorf("rollback Caddy: %w", rollback))
				return
			}
		}
		if quiesced {
			if rollback := d.ops.admin(ctx, old, "activate"); rollback != nil {
				err = errors.Join(err, fmt.Errorf("restore old slot: %w", rollback))
			}
		}
		if rollback := d.ops.stop(ctx, newSlot); rollback != nil {
			err = errors.Join(err, fmt.Errorf("stop failed standby: %w", rollback))
		}
	}()
	if err = d.ops.start(ctx, newSlot, digest); err != nil {
		return err
	}
	started = true
	// launchctl returns before the slot listens.
	standby, stateErr := waitSlot(ctx, d.ops, newSlot, modeStandby, d.readyTimeout)
	if stateErr != nil {
		return fmt.Errorf("new slot did not become standby: %v", stateErr)
	}
	if err = d.ops.admin(ctx, old, "snapshot"); err != nil {
		return err
	}
	if err = d.ops.admin(ctx, old, "quiesce"); err != nil {
		return err
	}
	quiesced = true
	if err = d.ops.admin(ctx, newSlot, "activate"); err != nil {
		return err
	}
	active, stateErr := d.ops.state(ctx, newSlot)
	if stateErr != nil || active.Mode != modeActive || active.PID != standby.PID {
		return fmt.Errorf("new slot did not activate: %v", stateErr)
	}
	// Until the flip the marker names the old slot, so a launchd restart of
	// it comes back active; switching then would leave two writers.
	prior, stateErr := d.ops.state(ctx, old)
	if stateErr != nil || prior.PID != current.PID || prior.Mode != modeQuiesced {
		return fmt.Errorf("old slot changed before the switch (pid %d mode %s): %v", prior.PID, prior.Mode, stateErr)
	}
	if err = d.ops.flip(ctx, newSlot); err != nil {
		return err
	}
	flipped = true
	if err = d.ready(ctx, newSlot, standby.PID); err != nil {
		return err
	}
	// The new slot serves now. Beyond this point nothing rolls back: a failure
	// leaves the old slot to the next deploy's retire.
	committed = true
	if err = d.ops.admin(ctx, old, "drain"); err != nil {
		return err
	}
	if err = d.waitDrain(ctx, old); err != nil {
		return err
	}
	if err = d.ops.stop(ctx, old); err != nil {
		return err
	}
	return d.ops.save(ctx, newSlot)
}

// retire drains and unloads a slot that Caddy no longer routes to. Standby
// holds no requests; a slot nothing answers for may still have a launchd
// label, and unloading a label that is not there is fine.
func (d *deployController) retire(ctx context.Context, slot string) error {
	state, err := d.ops.state(ctx, slot)
	if err != nil || state.PID == 0 {
		d.ops.stop(ctx, slot)
		return nil
	}
	if state.Mode == modeActive || state.Mode == modeQuiesced {
		if err := d.ops.admin(ctx, slot, "drain"); err != nil {
			return err
		}
	}
	if err := d.waitDrain(ctx, slot); err != nil {
		return err
	}
	return d.ops.stop(ctx, slot)
}

// waitDrain returns once the slot has no request in flight. A slot launchd
// restarted comes back standby or not listening yet; either way the process
// that held the requests is gone.
func (d *deployController) waitDrain(ctx context.Context, slot string) error {
	for {
		state, err := d.ops.state(ctx, slot)
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil
		}
		if err != nil {
			return err
		}
		if state.Mode == modeStandby || state.Mode == modeDraining && state.Pending == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
