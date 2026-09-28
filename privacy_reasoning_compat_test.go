package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"localrouter/internal/history"
	"localrouter/internal/privacy"
)

func TestPrivacyReasoningResponsesAcrossRoutes(t *testing.T) {
	seedTwoConnections(t)
	for _, protocol := range []string{"anthropic", "openai", "codex"} {
		for _, mode := range []string{"detect", "bypass", "mask"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				var calls atomic.Int32
				response := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"reasoning-check\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"answer-check\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
				if protocol == "anthropic" {
					response = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"reasoning-check\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"signature-check\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				} else if protocol == "codex" {
					response = "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"reasoning-check\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"answer-check\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r-check\",\"status\":\"completed\"}}\n\n"
				}
				if protocol == "anthropic" {
					response = strings.Replace(response, "event: message_stop", "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"answer-check\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\nevent: message_stop", 1)
				}
				responseWithAnswer := func(wire []byte) string {
					var payload any
					if err := json.Unmarshal(wire, &payload); err != nil {
						t.Error(err)
						return response
					}
					var answer string
					var visit func(any)
					visit = func(value any) {
						switch v := value.(type) {
						case string:
							if strings.HasPrefix(v, "password=") {
								answer = v
							}
						case []any:
							for _, child := range v {
								visit(child)
							}
						case map[string]any:
							for _, child := range v {
								visit(child)
							}
						}
					}
					visit(payload)
					if answer == "" || strings.Contains(answer, trafficCanary) == (mode == "mask") {
						t.Error("supported request text was not processed for selected mode")
					}
					encoded, _ := json.Marshal(answer)
					return strings.ReplaceAll(response, "answer-check", string(encoded[1:len(encoded)-1]))
				}
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					wire, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, responseWithAnswer(wire))
				}))
				defer up.Close()
				providerName, baseURL := "p", up.URL
				if protocol == "codex" {
					providerName, baseURL = "codex", codexBaseURL
					old := http.DefaultTransport
					t.Cleanup(func() { http.DefaultTransport = old })
					http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
						calls.Add(1)
						wire, err := io.ReadAll(r.Body)
						if err != nil {
							return nil, err
						}
						return usageResponse(200, responseWithAnswer(wire)), nil
					})
				}
				home := t.TempDir()
				filterMode := privacy.ModeMask
				if mode == "detect" {
					filterMode = privacy.ModeDetect
				}
				profiles := privacy.Profiles{Version: 1, Enabled: true, Default: "test", Profiles: []privacy.FilterProfile{{ID: "test", Name: "Test", Enabled: mode != "bypass", Mode: filterMode, Rules: json.RawMessage(`{}`)}}}
				encoded, err := json.Marshal(profiles)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(home, "privacy-profiles.json"), encoded, 0600); err != nil {
					t.Fatal(err)
				}
				base, err := url.Parse(baseURL)
				if err != nil {
					t.Fatal(err)
				}
				route := modelRoute{Mode: "model", Model: providerName + "/good"}
				if protocol == "anthropic" {
					route = modelRoute{Mode: "anthropic"}
				}
				providerType := ""
				if protocol == "codex" {
					providerType = "codex"
				}
				cfg := config{upstream: base, firstByte: time.Second, local: localSetup{Providers: []provider{{Name: providerName, Type: providerType, BaseURL: baseURL}}, Models: []localModel{{Provider: providerName, Model: "good"}}, Routes: map[string]map[string]modelRoute{"test": {"default": route}}}}
				cs := newConfigStore(cfg, filepath.Join(home, "providers.json"))
				st, hl := history.New(10, ""), newHealth("")
				h := newMainHandler(cfg, cs, st, hl, newUIServer(st, cs, hl))
				body := strings.Replace(trafficBody, `"max_tokens":100`, `"max_tokens":100,"stream":true`, 1)
				body = strings.TrimSuffix(body, "}") + `,"thinking":{"type":"enabled","budget_tokens":16},"context_management":{"edits":[]}}`
				result := trafficCall(h, "/v1/messages?beta=true", body)
				want := http.StatusOK
				if result.Code != want || calls.Load() != 1 {
					t.Fatalf("status=%d calls=%d, want %d/1: %s", result.Code, calls.Load(), want, result.Body)
				}
				if !strings.Contains(result.Body.String(), "reasoning-check") || !strings.Contains(result.Body.String(), "message_stop") {
					t.Fatal("reasoning or terminal lost")
				}
				if !strings.Contains(result.Body.String(), "password="+trafficCanary) {
					t.Fatal("supported answer text was not restored")
				}
				if protocol == "anthropic" {
					expected := strings.ReplaceAll(response, "answer-check", "password="+trafficCanary)
					if result.Body.String() != expected {
						t.Fatal("direct opaque response changed")
					}
				}
				if len(st.List()) != 0 {
					t.Fatal("reasoning request captured in history")
				}
			})
		}
	}
}
