package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type deployConfig struct {
	PublicAPI, PublicUI string
	BlueAPI, BlueUI     string
	GreenAPI, GreenUI   string
}

func (c deployConfig) validate(test bool) error {
	addresses := []string{c.PublicAPI, c.PublicUI, c.BlueAPI, c.BlueUI, c.GreenAPI, c.GreenUI}
	seen := make(map[string]bool)
	for _, address := range addresses {
		if strings.HasPrefix(address, "http://") {
			address = strings.TrimPrefix(address, "http://")
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || port == "0" || seen[address] {
			return fmt.Errorf("invalid or duplicate loopback address %q", address)
		}
		if test {
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
	config deployConfig
	ops    deployOps
	client *http.Client
}

func (d *deployController) validate(test bool) error { return d.config.validate(test) }

func (d *deployController) ready(ctx context.Context, slot string, pid int) error {
	client := d.client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(d.config.PublicAPI, "/")+"/healthz", nil)
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
	if err := d.validate(false); err != nil {
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
	other, stateErr := d.ops.state(ctx, newSlot)
	if current.Digest == digest && !force {
		if stateErr == nil && other.Mode == modeDraining && other.PID != 0 {
			if err = d.waitDrain(ctx, newSlot); err != nil {
				return err
			}
			if err = d.ops.stop(ctx, newSlot); err != nil {
				return err
			}
			return d.ops.save(ctx, old)
		}
		return nil
	}
	started, quiesced, flipped := false, false, false
	defer func() {
		if err == nil || !started {
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
	standby, stateErr := d.ops.state(ctx, newSlot)
	if stateErr != nil || standby.Mode != modeStandby || standby.PID == 0 {
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
	if err = d.ops.flip(ctx, newSlot); err != nil {
		return err
	}
	flipped = true
	if err = d.ready(ctx, newSlot, standby.PID); err != nil {
		return err
	}
	// Beyond this point the old instance is draining; no rollback is possible.
	flipped, quiesced = false, false
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

func (d *deployController) waitDrain(ctx context.Context, slot string) error {
	for {
		state, err := d.ops.state(ctx, slot)
		if err != nil {
			return err
		}
		if state.Mode == modeDraining && state.Pending == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func parseLoopbackAddress(address string) (string, error) {
	if strings.Contains(address, "://") {
		u, err := url.Parse(address)
		if err != nil {
			return "", err
		}
		address = u.Host
	}
	ip, _, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(ip) == nil || !net.ParseIP(ip).IsLoopback() {
		return "", fmt.Errorf("not a loopback address: %s", address)
	}
	return address, nil
}
