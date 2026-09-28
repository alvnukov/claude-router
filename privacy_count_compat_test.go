package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPrivacyCountUsesSelectedRouteWithoutGeneration(t *testing.T) {
	for _, local := range []bool{false, true} {
		for _, mode := range []string{"detect", "bypass", "mask"} {
			t.Run(map[bool]string{false: "anthropic", true: "local"}[local]+"/"+mode, func(t *testing.T) {
				var calls atomic.Int32
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if local || r.URL.Path != "/v1/messages/count_tokens" || r.URL.RawQuery != "beta=true" {
						t.Error("token count generated a completion or used the wrong endpoint")
					}
					wire, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					var payload map[string]json.RawMessage
					if err := json.Unmarshal(wire, &payload); err != nil {
						t.Error(err)
					}
					if bytes.Contains(payload["messages"], []byte(trafficCanary)) != (mode != "mask") {
						t.Error("count request protection mode incorrect")
					}
					if !bytes.Contains(wire, []byte(`"thinking":{"type":"enabled","budget_tokens":16}`)) || !bytes.Contains(wire, []byte(`"context_management":{"edits":[]}`)) {
						t.Error("count controls changed")
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"input_tokens":12}`)
				}))
				defer up.Close()
				h, history, home := trafficFixture(t, up, local)
				file := filepath.Join(home, "privacy-profiles.json")
				config, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "detect" {
					config = bytes.Replace(config, []byte(`"rules":`), []byte(`"mode":"detect","rules":`), 1)
				} else if mode == "bypass" {
					config = bytes.Replace(config, []byte(`"name":"Safe","enabled":true`), []byte(`"name":"Safe","enabled":false`), 1)
				}
				if err := os.WriteFile(file, config, 0600); err != nil {
					t.Fatal(err)
				}
				body := strings.TrimSuffix(trafficBody, "}") + `,"thinking":{"type":"enabled","budget_tokens":16},"context_management":{"edits":[]}}`
				response := trafficCall(h, "/v1/messages/count_tokens?beta=true", body)
				wantStatus, wantCalls := http.StatusOK, int32(0)
				if !local {
					wantCalls = 1
				}
				if response.Code != wantStatus || calls.Load() != wantCalls {
					t.Fatalf("status=%d calls=%d, want %d/%d: %s", response.Code, calls.Load(), wantStatus, wantCalls, response.Body)
				}
				if wantStatus == http.StatusOK {
					var count struct {
						InputTokens int `json:"input_tokens"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &count); err != nil {
						t.Fatal(err)
					}
					want := 12
					if local {
						want = len(body) / 4
					}
					if count.InputTokens != want {
						t.Fatalf("count=%d, want %d", count.InputTokens, want)
					}
				}
				if len(history.List()) != 0 {
					t.Fatal("token count captured private body in history")
				}
			})
		}
	}
}

func TestPrivacyCountRejectsInvalidUpstreamResponse(t *testing.T) {
	for _, response := range []string{`{}`, `{"input_tokens":-1}`, `{"input_tokens":"12"}`, `{"input_tokens":1.5}`, `{"input_tokens":9223372036854775808}`, `{"input_tokens":1,"input_tokens":2}`, `{"input_tokens":null}`, `{"input_tokens":2}garbage`} {
		t.Run(response, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, response)
			}))
			defer up.Close()
			h, _, home := trafficFixture(t, up, false)
			file := filepath.Join(home, "privacy-profiles.json")
			config, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			config = bytes.Replace(config, []byte(`"rules":`), []byte(`"mode":"detect","rules":`), 1)
			if err := os.WriteFile(file, config, 0600); err != nil {
				t.Fatal(err)
			}
			result := trafficCall(h, "/v1/messages/count_tokens", trafficBody)
			if result.Code != http.StatusBadGateway || strings.Contains(result.Body.String(), "input_tokens") {
				t.Fatalf("invalid token count released: %d %s", result.Code, result.Body)
			}
		})
	}
}
