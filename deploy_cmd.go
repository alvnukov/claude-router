package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

// deployFile is <home>/deploy.json, written by install --cutover. It is the
// only place the live public, slot and Caddy admin addresses are named.
type deployFile struct {
	CaddyAdmin string `json:"caddy_admin"`
	Caddy      string `json:"caddy,omitempty"` // absolute path; launchd has no PATH to find it by
	deployConfig
}

type deployOpsFactory func(file deployFile, home, agents, binary string) deployOps

func newDeployOps(file deployFile, home, agents, binary string) deployOps {
	ops := newSystemDeployOps(file.deployConfig, "http://"+file.CaddyAdmin, home, agents, binary)
	if file.Caddy != "" {
		ops.caddy = file.Caddy
	}
	return ops
}

// validate checks the Caddy admin address as well as the router ports.
func (f deployFile) validate() error {
	if host, _, err := net.SplitHostPort(f.CaddyAdmin); err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("Caddy admin must be a loopback host:port, got %q", f.CaddyAdmin)
	}
	return f.deployConfig.validate()
}

func fileDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// withDeployLock runs fn under <home>/deploy.lock, tried once: a second deploy
// fails at once instead of queueing behind a drain that may take minutes.
func withDeployLock(ctx context.Context, home string, fn func() error) error {
	try, stop := context.WithCancel(ctx)
	stop()
	locked := false
	err := withFileLock(try, filepath.Join(home, "deploy.lock"), func() error {
		locked = true
		return fn()
	})
	if !locked && errors.Is(err, context.Canceled) {
		return errors.New("another deploy or cutover is running")
	}
	return err
}

// runDeploy is `localrouter deploy`: switch the serving slot to -binary
// without dropping a request. One deploy runs at a time.
func runDeploy(ctx context.Context, args []string, out io.Writer, newOps deployOpsFactory) error {
	home, _ := os.UserHomeDir()
	flags := flag.NewFlagSet("deploy", flag.ContinueOnError)
	flags.SetOutput(out)
	dir := flags.String("home", env("ROUTER_HOME", filepath.Join(home, ".claude/local-router")), "router home with deploy.json, slot binaries and state")
	agents := flags.String("agents", filepath.Join(home, "Library/LaunchAgents"), "launchd agents directory for the slot plists")
	binary := flags.String("binary", "", "candidate router binary")
	force := flags.Bool("force", false, "switch slots even when the active slot runs this binary")
	drain := flags.Duration("drain-timeout", 15*time.Minute, "how long the old slot may take to finish its requests")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *binary == "" {
		return errors.New("deploy: -binary is required")
	}
	data, err := os.ReadFile(filepath.Join(*dir, "deploy.json"))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("deploy: no %s; run ./router install --cutover first", filepath.Join(*dir, "deploy.json"))
	}
	if err != nil {
		return err
	}
	var file deployFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("deploy.json: %w", err)
	}
	if err := file.validate(); err != nil {
		return fmt.Errorf("deploy.json: %w", err)
	}
	digest, err := fileDigest(*binary)
	if err != nil {
		return err
	}

	ops := newOps(file, *dir, *agents, *binary)
	controller := &deployController{config: file.deployConfig, ops: ops}
	ctx, cancel := context.WithTimeout(ctx, *drain+2*time.Minute)
	defer cancel()
	return withDeployLock(ctx, *dir, func() error {
		if err := controller.deploy(ctx, digest, *force); err != nil {
			return err
		}
		slot, err := ops.current(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "active slot %s runs %s\n", slot, digest[:12])
		return nil
	})
}
