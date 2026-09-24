package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type systemDeployOps struct {
	config   deployConfig
	adminURL string
	home     string
	agents   string
	binary   string
	client   *http.Client
}

func newSystemDeployOps(config deployConfig, adminURL, home, agents, binary string) *systemDeployOps {
	return &systemDeployOps{config: config, adminURL: adminURL, home: home, agents: agents, binary: binary, client: &http.Client{Timeout: 5 * time.Second}}
}

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

func (o *systemDeployOps) caddyfile(slot string) string {
	return fmt.Sprintf(`{
    admin %s
}
http://%s {
    reverse_proxy %s {
        flush_interval -1
        stream_close_delay 16m
    }
}
http://%s {
    reverse_proxy %s {
        flush_interval -1
        stream_close_delay 16m
    }
}
`, strings.TrimPrefix(o.adminURL, "http://"), o.config.PublicAPI, o.address(slot, false), o.config.PublicUI, o.address(slot, true))
}

type launchdSlotPlist struct {
	XMLName              xml.Name `xml:"plist"`
	Version              string   `xml:"version,attr"`
	Label                string
	ProgramArguments     []string `xml:"ProgramArguments>string"`
	RunAtLoad            bool
	KeepAlive            bool
	ExitTimeOut          int
	EnvironmentVariables map[string]string
	StandardOutPath      string
	StandardErrorPath    string
}

func (o *systemDeployOps) slotPlist(slot string) launchdSlotPlist {
	log := filepath.Join(o.home, "router."+slot+".log")
	return launchdSlotPlist{Version: "1.0", Label: "com.claude-local-router." + slot, ProgramArguments: []string{o.binary}, RunAtLoad: true, KeepAlive: true, ExitTimeOut: 960,
		EnvironmentVariables: map[string]string{"ROUTER_SLOT": slot, "ROUTER_STANDBY": "1", "ROUTER_LISTEN": o.address(slot, false), "ROUTER_UI_LISTEN": o.address(slot, true), "ROUTER_PROVIDERS_FILE": filepath.Join(o.home, "providers.json"), "ROUTER_ENV_FILE": filepath.Join(o.home, "env"), "ROUTER_STATE_FILE": filepath.Join(o.home, "state.json"), "ROUTER_UI_HISTORY_FILE": filepath.Join(o.home, "history.jsonl")}, StandardOutPath: log, StandardErrorPath: log}
}

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

func (o *systemDeployOps) flip(ctx context.Context, slot string) error {
	file := filepath.Join(o.home, "Caddyfile")
	candidate := file + ".next"
	if err := os.WriteFile(candidate, []byte(o.caddyfile(slot)), 0600); err != nil {
		return err
	}
	defer os.Remove(candidate)
	cmd := exec.CommandContext(ctx, "caddy", "adapt", "--config", candidate, "--adapter", "caddyfile")
	config, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("adapt Caddy config: %w", err)
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
	return nil
}

func (o *systemDeployOps) save(_ context.Context, slot string) error {
	file := filepath.Join(o.home, "Caddyfile")
	candidate := file + ".next"
	if err := os.WriteFile(candidate, []byte(o.caddyfile(slot)), 0600); err != nil {
		return err
	}
	return os.Rename(candidate, file)
}

func (o *systemDeployOps) start(ctx context.Context, slot, digest string) error {
	return fmt.Errorf("starting slot %s for %s is not configured", slot, digest)
}

func (o *systemDeployOps) stop(ctx context.Context, slot string) error {
	return fmt.Errorf("stopping slot %s is not configured", slot)
}
