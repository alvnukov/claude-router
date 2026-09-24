package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type systemDeployOps struct {
	config   deployConfig
	adminURL string
	home     string
	agents   string
	binary   string
	client   *http.Client
	// launchctl is replaced in tests so they never touch the user's launchd.
	launchctl func(context.Context, ...string) error
}

func newSystemDeployOps(config deployConfig, adminURL, home, agents, binary string) *systemDeployOps {
	return &systemDeployOps{config: config, adminURL: adminURL, home: home, agents: agents, binary: binary, client: &http.Client{Timeout: 5 * time.Second}, launchctl: runLaunchctl}
}

func runLaunchctl(ctx context.Context, args ...string) error {
	if testing.Testing() {
		return fmt.Errorf("launchctl %s: refused in a test binary", strings.Join(args, " "))
	}
	output, err := exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(output))
	}
	return nil
}

func slotLabel(slot string) string { return "com.claude-local-router." + slot }

func launchdDomain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func (o *systemDeployOps) address(slot string, ui bool) string {
	if slot == "blue" {
		if ui {
			return o.config.BlueUI
		}
		return o.config.BlueAPI
	}
	if ui {
		return o.config.GreenUI
	}
	return o.config.GreenAPI
}

func (o *systemDeployOps) current(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.adminURL+"/config/", nil)
	if err != nil {
		return "", err
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Caddy config: HTTP %d", resp.StatusCode)
	}
	var config struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Listen []string `json:"listen"`
					Routes []struct {
						Handle []struct {
							Handler   string `json:"handler"`
							Upstreams []struct {
								Dial string `json:"dial"`
							} `json:"upstreams"`
						} `json:"handle"`
					} `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		return "", err
	}
	upstream := func(public string) string {
		for _, server := range config.Apps.HTTP.Servers {
			for _, listen := range server.Listen {
				if listen != public {
					continue
				}
				for _, route := range server.Routes {
					for _, handler := range route.Handle {
						if handler.Handler == "reverse_proxy" && len(handler.Upstreams) == 1 {
							return handler.Upstreams[0].Dial
						}
					}
				}
			}
		}
		return ""
	}
	api, ui := upstream(o.config.PublicAPI), upstream(o.config.PublicUI)
	for _, slot := range []string{"blue", "green"} {
		if api == o.address(slot, false) && ui == o.address(slot, true) {
			return slot, nil
		}
	}
	return "", fmt.Errorf("Caddy upstreams disagree or are unknown: API %q UI %q", api, ui)
}

// caddyfile names each public listener by port and binds it to its loopback
// host: a host in the site address would bind every interface and add a Host
// matcher, and the UI is opened as localhost as well as 127.0.0.1.
func (o *systemDeployOps) caddyfile(slot string) string {
	site := func(public, upstream string) string {
		host, port, _ := net.SplitHostPort(public)
		return fmt.Sprintf(`http://:%s {
    bind %s
    reverse_proxy %s {
        flush_interval -1
        stream_close_delay 16m
    }
}
`, port, host, upstream)
	}
	return fmt.Sprintf("{\n    admin %s\n}\n", strings.TrimPrefix(o.adminURL, "http://")) +
		site(o.config.PublicAPI, o.address(slot, false)) + site(o.config.PublicUI, o.address(slot, true))
}

type launchdSlotPlist struct {
	Label                string
	ProgramArguments     []string
	WorkingDirectory     string
	RunAtLoad            bool
	KeepAlive            bool
	ExitTimeOut          int
	EnvironmentVariables map[string]string
	StandardOutPath      string
	StandardErrorPath    string
}

func plistEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (p launchdSlotPlist) xml() []byte {
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
	str("Label", p.Label)
	b.WriteString("  <key>ProgramArguments</key>\n  <array>")
	for _, arg := range p.ProgramArguments {
		fmt.Fprintf(&b, "<string>%s</string>", plistEscape(arg))
	}
	b.WriteString("</array>\n")
	str("WorkingDirectory", p.WorkingDirectory)
	boolean("RunAtLoad", p.RunAtLoad)
	boolean("KeepAlive", p.KeepAlive)
	fmt.Fprintf(&b, "  <key>ExitTimeOut</key><integer>%d</integer>\n", p.ExitTimeOut)
	b.WriteString("  <key>ProcessType</key><string>Background</string>\n")
	b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
	keys := make([]string, 0, len(p.EnvironmentVariables))
	for key := range p.EnvironmentVariables {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&b, "    <key>%s</key><string>%s</string>\n", plistEscape(key), plistEscape(p.EnvironmentVariables[key]))
	}
	b.WriteString("  </dict>\n")
	str("StandardOutPath", p.StandardOutPath)
	str("StandardErrorPath", p.StandardErrorPath)
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

// slotPlist runs the slot's own copy of the binary. The slot decides at start
// whether it is active from the active-slot marker, so a KeepAlive restart of
// the serving slot comes back active and a fresh candidate comes up standby.
func (o *systemDeployOps) slotPlist(slot string) launchdSlotPlist {
	log := filepath.Join(o.home, "router."+slot+".log")
	return launchdSlotPlist{Label: slotLabel(slot), ProgramArguments: []string{filepath.Join(o.home, "localrouter."+slot)}, WorkingDirectory: o.home, RunAtLoad: true, KeepAlive: true, ExitTimeOut: 960,
		EnvironmentVariables: map[string]string{"ROUTER_SLOT": slot, "ROUTER_ACTIVE_SLOT_FILE": o.markerPath(), "ROUTER_LISTEN": o.address(slot, false), "ROUTER_UI_LISTEN": o.address(slot, true), "ROUTER_PROVIDERS_FILE": filepath.Join(o.home, "providers.json"), "ROUTER_ENV_FILE": filepath.Join(o.home, "env"), "ROUTER_STATE_FILE": filepath.Join(o.home, "state.json"), "ROUTER_UI_HISTORY_FILE": filepath.Join(o.home, "history.jsonl")}, StandardOutPath: log, StandardErrorPath: log}
}

func (o *systemDeployOps) markerPath() string { return filepath.Join(o.home, "active-slot") }

func (o *systemDeployOps) state(ctx context.Context, slot string) (deploySlotState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+o.address(slot, false)+"/healthz", nil)
	if err != nil {
		return deploySlotState{}, err
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return deploySlotState{}, err
	}
	defer resp.Body.Close()
	var state deploySlotState
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return state, err
	}
	if resp.StatusCode != http.StatusOK && state.Mode != modeDraining {
		return state, fmt.Errorf("slot %s: HTTP %d", slot, resp.StatusCode)
	}
	if state.Slot != slot || state.PID == 0 {
		return state, fmt.Errorf("unexpected slot identity: %+v", state)
	}
	binary := filepath.Join(o.home, "localrouter."+slot)
	data, err := os.ReadFile(binary)
	if err != nil {
		return state, err
	}
	hash := sha256.Sum256(data)
	state.Digest = hex.EncodeToString(hash[:])
	return state, nil
}

func (o *systemDeployOps) admin(ctx context.Context, slot, action string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+o.address(slot, false)+"/admin/"+action, nil)
	if err != nil {
		return err
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%s %s: HTTP %d: %s", slot, action, resp.StatusCode, body)
	}
	return nil
}

func (o *systemDeployOps) adapt(ctx context.Context, path string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, "caddy", "adapt", "--config", path, "--adapter", "caddyfile").Output()
	if err != nil {
		return nil, fmt.Errorf("adapt Caddy config: %w", err)
	}
	return output, nil
}

func (o *systemDeployOps) flip(ctx context.Context, slot string) error {
	file := filepath.Join(o.home, "Caddyfile")
	candidate := file + ".next"
	if err := os.WriteFile(candidate, []byte(o.caddyfile(slot)), 0600); err != nil {
		return err
	}
	defer os.Remove(candidate)
	config, err := o.adapt(ctx, candidate)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.adminURL+"/load", bytes.NewReader(config))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("Caddy flip: HTTP %d: %s", resp.StatusCode, body)
	}
	// Persist at once: a Caddy or slot restart before the deploy finishes must
	// come back to the slot that is serving now.
	return o.save(ctx, slot)
}

// save makes the on-disk Caddyfile and the active-slot marker name slot.
func (o *systemDeployOps) save(_ context.Context, slot string) error {
	if err := writeFileAtomic(filepath.Join(o.home, "Caddyfile"), []byte(o.caddyfile(slot)), 0o600); err != nil {
		return err
	}
	return writeFileAtomic(o.markerPath(), []byte(slot+"\n"), 0o600)
}

// start installs the candidate as the slot's own binary and loads the slot's
// launchd label. The slot starts in standby; only /admin/activate lets it write.
func (o *systemDeployOps) start(ctx context.Context, slot, digest string) error {
	data, err := os.ReadFile(o.binary)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(data)
	if got := hex.EncodeToString(hash[:]); got != digest {
		return fmt.Errorf("candidate digest %s, want %s", got, digest)
	}
	if err := writeFileAtomic(filepath.Join(o.home, "localrouter."+slot), data, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(o.agents, 0o755); err != nil {
		return err
	}
	plist := filepath.Join(o.agents, slotLabel(slot)+".plist")
	if err := writeFileAtomic(plist, o.slotPlist(slot).xml(), 0o644); err != nil {
		return err
	}
	return o.launchctl(ctx, "bootstrap", launchdDomain(), plist)
}

// stop unloads a slot label. Callers drain the slot first, so launchd's
// SIGTERM finds no request in flight; ExitTimeOut covers the rest.
func (o *systemDeployOps) stop(ctx context.Context, slot string) error {
	return o.launchctl(ctx, "bootout", launchdDomain()+"/"+slotLabel(slot))
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
