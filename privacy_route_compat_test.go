package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"localrouter/internal/history"
	"localrouter/internal/privacy"
)

// All provider requests use an in-memory transport. Only trafficCall's client
// connects to the loopback router; these tests never contact provider APIs.
func routeCompatRequest(t *testing.T, protocol, routeKind, mode, controls string, bindings map[string]string, failFirst bool, requestPath ...string) (int, []string) {
	t.Helper()
	providerName, baseURL := "p", "http://privacy-route.invalid/v1"
	if protocol == "codex" {
		providerName, baseURL = "codex", codexBaseURL
	}
	var models []string
	oldTransport := http.DefaultTransport
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.String() != baseURL+map[string]string{"openai": "/chat/completions", "codex": "/responses"}[protocol] {
			return nil, fmt.Errorf("unexpected test request: %s %s", r.Method, r.URL)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		var model string
		if err := json.Unmarshal(payload["model"], &model); err != nil {
			return nil, err
		}
		models = append(models, model)
		if payload["thinking"] != nil || payload["context_management"] != nil {
			t.Error("Anthropic controls escaped the normal translator")
		}
		selectedMode := mode
		if selected, ok := bindings[model]; ok {
			selectedMode = selected
		}
		masked := mode != "off" && selectedMode == "mask"
		if strings.Contains(string(body), trafficCanary) == masked {
			t.Error("actual target protection mode was not applied")
		}
		if failFirst && len(models) == 1 {
			return usageResponse(http.StatusServiceUnavailable, `{"error":"temporary"}`), nil
		}
		if protocol == "codex" {
			return usageResponse(http.StatusOK, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"compat-response\",\"status\":\"completed\"}}\n\n"), nil
		}
		return usageResponse(http.StatusOK, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`), nil
	})
	t.Cleanup(func() { http.DefaultTransport = oldTransport })

	home := t.TempDir()
	policy := privacy.Profiles{Version: 1, Enabled: mode != "off", Default: mode}
	for _, name := range []string{"off", "detect", "mask", "bypass"} {
		filterMode := privacy.ModeMask
		if name == "detect" {
			filterMode = privacy.ModeDetect
		}
		policy.Profiles = append(policy.Profiles, privacy.FilterProfile{ID: name, Name: name, Enabled: name != "bypass", Mode: filterMode, Rules: json.RawMessage(`{}`)})
	}
	for model, profile := range bindings {
		policy.Bindings = append(policy.Bindings, privacy.Binding{Kind: "model", Target: providerName + "/" + model, Profile: profile})
	}
	encoded, err := json.Marshal(policy)
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
	setup := localSetup{Providers: []provider{{Name: providerName, BaseURL: baseURL}}, Models: []localModel{{Provider: providerName, Model: "chosen"}, {Provider: providerName, Model: "other"}}}
	if protocol == "codex" {
		setup.Providers[0].Type = "codex"
	}
	route := modelRoute{Mode: "model", Model: providerName + "/chosen"}
	if routeKind == "pool" || routeKind == "default" {
		route = modelRoute{Mode: "pool", Pool: "selected"}
		setup.ModelPools = map[string][]poolTarget{"selected": {{Model: providerName + "/chosen"}, {Model: providerName + "/other"}}}
	}
	model := "test"
	switch routeKind {
	case "default":
		setup.DefaultPool = "selected"
	case "family":
		model = "claude-opus-5"
		setup.FamilyRoutes = map[string]map[string]modelRoute{"opus": {"default": route}}
	default:
		setup.Routes = map[string]map[string]modelRoute{model: {"default": route}}
	}
	cfg := config{upstream: base, firstByte: time.Second, failover: true, local: setup}
	cs := newConfigStore(cfg, filepath.Join(home, "providers.json"))
	st, hl := history.New(10, ""), newHealth("")
	handler := newMainHandler(cfg, cs, st, hl, newUIServer(st, cs, hl))
	body := strings.Replace(trafficBody, `"model":"test"`, `"model":"`+model+`"`, 1)
	if controls == "thinking" || controls == "both" {
		body = strings.TrimSuffix(body, "}") + `,"thinking":{"type":"enabled","budget_tokens":32}}`
	}
	if controls == "context" || controls == "both" {
		body = strings.TrimSuffix(body, "}") + `,"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}}`
	}
	path := "/v1/messages"
	if len(requestPath) != 0 {
		path = requestPath[0]
	}
	response := trafficCall(handler, path+"?beta=true", body)
	if path == "/v1/messages/count_tokens" && response.Code == http.StatusOK {
		var count struct {
			InputTokens int `json:"input_tokens"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &count); err != nil || count.InputTokens != len(body)/4 {
			t.Fatalf("invalid local estimate: %s (%v)", response.Body, err)
		}
		selectedMode := mode
		if selected, ok := bindings["chosen"]; ok {
			selectedMode = selected
		}
		state := cs.privacyRuntime().State()
		var protected, detected, bypassed int
		switch selectedMode {
		case "mask":
			protected = 1
		case "detect":
			detected = 1
		case "bypass":
			bypassed = 1
		}
		if state.Protected != protected || state.Detected != detected || state.Bypassed != bypassed || state.Rejected != 0 || state.Active != 0 {
			t.Fatalf("count did not resolve selected target %s: %+v", selectedMode, state)
		}

	}
	if mode != "off" && len(st.List()) != 0 {
		t.Error("protected traffic entered history")
	}
	return response.Code, models
}

func TestPrivacyTranslatedCountResolvesActualTarget(t *testing.T) {
	seedTwoConnections(t)
	for _, protocol := range []string{"openai", "codex"} {
		for _, route := range []string{"model", "pool", "default", "family"} {
			for _, tc := range []struct {
				mode, selected string
				want           int
			}{
				{"detect", "detect", http.StatusOK}, {"mask", "detect", http.StatusOK},
				{"mask", "bypass", http.StatusOK}, {"detect", "mask", http.StatusOK},
			} {
				t.Run(protocol+"/"+route+"/"+tc.mode+"/"+tc.selected, func(t *testing.T) {
					status, calls := routeCompatRequest(t, protocol, route, tc.mode, "both", map[string]string{"chosen": tc.selected}, false, "/v1/messages/count_tokens")
					if status != tc.want || len(calls) != 0 {
						t.Fatalf("count status=%d calls=%v, want %d/no upstream", status, calls, tc.want)
					}
				})
			}
		}
	}
}

func TestPrivacyTranslatedRouteControls(t *testing.T) {
	seedTwoConnections(t)
	for _, protocol := range []string{"openai", "codex"} {
		for _, route := range []string{"model", "pool", "default", "family"} {
			for _, mode := range []string{"off", "detect", "bypass", "mask"} {
				for _, controls := range []string{"none", "thinking", "context", "both"} {
					t.Run(protocol+"/"+route+"/"+mode+"/"+controls, func(t *testing.T) {
						status, models := routeCompatRequest(t, protocol, route, mode, controls, nil, false)
						wantStatus, wantModels := http.StatusOK, []string{"chosen"}
						if status != wantStatus || !reflect.DeepEqual(models, wantModels) {
							t.Fatalf("status=%d targets=%v; want %d %v", status, models, wantStatus, wantModels)
						}
					})
				}
			}
		}
	}
}

func TestPrivacyTranslatedControlsResolveActualTarget(t *testing.T) {
	seedTwoConnections(t)
	for _, protocol := range []string{"openai", "codex"} {
		for _, tc := range []struct {
			name, mode, selectedMode string
			wantStatus               int
		}{
			{"detect overridden by mask", "detect", "mask", http.StatusOK},
			{"mask overridden by detect", "mask", "detect", http.StatusOK},
			{"mask overridden by bypass", "mask", "bypass", http.StatusOK},
			{"bypass overridden by mask", "bypass", "mask", http.StatusOK},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				status, models := routeCompatRequest(t, protocol, "pool", tc.mode, "both", map[string]string{"chosen": tc.selectedMode}, false)
				var wantModels []string
				if tc.wantStatus == http.StatusOK {
					wantModels = []string{"chosen"}
				}
				if status != tc.wantStatus || !reflect.DeepEqual(models, wantModels) {
					t.Fatalf("status=%d targets=%v; want %d %v", status, models, tc.wantStatus, wantModels)
				}
			})
		}
	}
}

func TestPrivacyTranslatedControlsResolveFailoverTarget(t *testing.T) {
	seedTwoConnections(t)
	for _, protocol := range []string{"openai", "codex"} {
		for _, mode := range []string{"detect", "bypass"} {
			for _, nextMode := range []string{"detect", "mask"} {
				t.Run(protocol+"/"+mode+"/next-"+nextMode, func(t *testing.T) {
					status, models := routeCompatRequest(t, protocol, "default", mode, "both", map[string]string{"other": nextMode}, true)
					wantStatus, wantModels := http.StatusOK, []string{"chosen", "other"}
					if status != wantStatus || !reflect.DeepEqual(models, wantModels) {
						t.Fatalf("status=%d targets=%v; want %d %v", status, models, wantStatus, wantModels)
					}
				})
			}
		}
	}
}
