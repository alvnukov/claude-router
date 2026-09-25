package platform

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Launchd is the Service of macOS: a user agent per label, defined by
// Dir/<label>.plist and loaded into Domain.
type Launchd struct {
	Dir    string // usually ~/Library/LaunchAgents
	Domain string // usually LaunchdDomain()
	// Run runs launchctl; tests replace it so they never touch the user's
	// launchd.
	Run func(ctx context.Context, args ...string) error
}

// LaunchdDomain is the GUI domain of the current user, gui/<uid>.
func LaunchdDomain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

// RunLaunchctl runs launchctl with args and returns its output in the error.
// It refuses in a test binary.
func RunLaunchctl(ctx context.Context, args ...string) error {
	if testing.Testing() {
		return fmt.Errorf("launchctl %s: refused in a test binary", strings.Join(args, " "))
	}
	output, err := exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(output))
	}
	return nil
}

var _ Service = Launchd{}

func (l Launchd) plist(label string) string { return filepath.Join(l.Dir, label+".plist") }

func (l Launchd) target(label string) string { return l.Domain + "/" + label }

func (l Launchd) Install(_ context.Context, spec ServiceSpec) error {
	if err := os.MkdirAll(l.Dir, 0o755); err != nil {
		return err
	}
	return WriteFileAtomic(l.plist(spec.Label), LaunchdPlist(spec), 0o644)
}

// Uninstall keeps the plist of an agent it could not unload, so the agent
// stays described and a retry finds it.
func (l Launchd) Uninstall(ctx context.Context, label string) error {
	if err := l.Stop(ctx, label); err != nil && l.loaded(ctx, label) {
		return err
	}
	if err := os.Remove(l.plist(label)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (l Launchd) Start(ctx context.Context, label string) error {
	return l.Run(ctx, "bootstrap", l.Domain, l.plist(label))
}

func (l Launchd) Stop(ctx context.Context, label string) error {
	return l.Run(ctx, "bootout", l.target(label))
}

func (l Launchd) Status(ctx context.Context, label string) (Status, error) {
	_, err := os.Stat(l.plist(label))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Status{}, err
	}
	return Status{Installed: err == nil, Loaded: l.loaded(ctx, label)}, nil
}

func (l Launchd) loaded(ctx context.Context, label string) bool {
	return l.Run(ctx, "print", l.target(label)) == nil
}

// LaunchdPlist renders spec as a launchd agent definition. The agent starts
// at load and runs as a background process; keys whose value is unset are
// left out so launchd applies its defaults.
func LaunchdPlist(spec ServiceSpec) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	str := func(key, value string) {
		fmt.Fprintf(&b, "  <key>%s</key><string>%s</string>\n", key, plistEscape(value))
	}
	boolean := func(key string, value bool) {
		fmt.Fprintf(&b, "  <key>%s</key><%t/>\n", key, value)
	}
	seconds := func(key string, d time.Duration) {
		if s := int64(d / time.Second); s > 0 {
			fmt.Fprintf(&b, "  <key>%s</key><integer>%d</integer>\n", key, s)
		}
	}
	str("Label", spec.Label)
	b.WriteString("  <key>ProgramArguments</key>\n  <array>")
	for _, arg := range append([]string{spec.Exe}, spec.Args...) {
		fmt.Fprintf(&b, "<string>%s</string>", plistEscape(arg))
	}
	b.WriteString("</array>\n")
	str("WorkingDirectory", spec.Dir)
	boolean("RunAtLoad", true)
	boolean("KeepAlive", spec.KeepAlive)
	seconds("ThrottleInterval", spec.ThrottleInterval)
	seconds("ExitTimeOut", spec.ExitTimeout)
	str("ProcessType", "Background")
	if len(spec.Env) > 0 {
		b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		keys := make([]string, 0, len(spec.Env))
		for key := range spec.Env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(&b, "    <key>%s</key><string>%s</string>\n", plistEscape(key), plistEscape(spec.Env[key]))
		}
		b.WriteString("  </dict>\n")
	}
	str("StandardOutPath", spec.LogPath)
	str("StandardErrorPath", spec.LogPath)
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

func plistEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s)) // a bytes.Buffer does not fail
	return b.String()
}
