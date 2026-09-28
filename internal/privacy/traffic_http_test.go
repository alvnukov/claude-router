package privacy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"localrouter/internal/anthropicerror"
	"localrouter/internal/providers/codex"
)

func compatHTTPBody() []byte { return clientCompatBody() }

func compatSSE(alias string) string {
	frame := func(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }
	text, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": "answer " + alias}})
	tool, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]string{"type": "input_json_delta", "partial_json": `{"query":"` + alias + `"}`}})
	return frame("message_start", `{"type":"message_start","message":{"id":"msg_synthetic","type":"message","role":"assistant","model":"synthetic-supported","content":[],"usage":{"input_tokens":17,"output_tokens":0}}}`) +
		frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
		frame("content_block_delta", string(text)) +
		frame("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		frame("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_synthetic","name":"run_synthetic","input":{}}}`) +
		frame("content_block_delta", string(tool)) +
		frame("content_block_stop", `{"type":"content_block_stop","index":1}`) +
		frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":8}}`) +
		frame("message_stop", `{"type":"message_stop"}`)
}

func compatHTTPDeps(t *testing.T, rt *Runtime, upstream string, mode string, localCalls, legacyCalls *atomic.Int32) HTTPDeps {
	t.Helper()
	endpoint, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	return HTTPDeps{
		Runtime: rt,
		Legacy: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			legacyCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"legacy":"off-path"}`)
		}),
		Resolve: func([]byte) (HTTPRoute, error) {
			return HTTPRoute{
				Mode: mode, Model: compatModel, Upstream: endpoint,
				Pool: func(string) (string, string) { return "synthetic-profile", "synthetic-pool" },
				Local: func(w http.ResponseWriter, _ *http.Request, _ []byte, _ string) {
					localCalls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"content":[]}`)
				},
			}, nil
		},
		Client: &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		Limits: LifecycleLimits{Inbound: 5 * time.Second, Headers: 5 * time.Second, Idle: 5 * time.Second, Total: 15 * time.Second, Cleanup: 3 * time.Second},
		WriteError: func(w http.ResponseWriter, status int, kind, message string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": message}})
		},
	}
}

type compatHTTPResult struct {
	Code   int
	Header http.Header
	Body   bytes.Buffer
}

func compatHTTPCall(t *testing.T, handler http.Handler, path string, body []byte) *compatHTTPResult {
	t.Helper()
	return compatHTTPCallWithHeaders(t, handler, path, body, nil)
}

func compatHTTPCallWithHeaders(t *testing.T, handler http.Handler, path string, body []byte, extra http.Header) *compatHTTPResult {
	t.Helper()
	server := httptest.NewServer(handler) // real ResponseController; Recorder cannot set write deadlines
	defer server.Close()
	r, err := http.NewRequest(http.MethodPost, server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Anthropic-Version", "2023-06-01")
	r.Header.Set("Anthropic-Beta", compatBeta)
	r.Header.Set("X-Api-Key", "client-only-synthetic-key")
	for key, values := range extra {
		r.Header[key] = values
	}
	client := server.Client()
	client.Transport = &http.Transport{Proxy: nil} // only this loopback server
	client.Timeout = 20 * time.Second
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	result := &compatHTTPResult{Code: resp.StatusCode, Header: resp.Header.Clone()}
	if _, err := result.Body.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestProtectedHTTPClientFirst(t *testing.T) {
	t.Run("mask-direct-first-turn", func(t *testing.T) {
		var upstreamCalls, localCalls, legacyCalls atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamCalls.Add(1)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			if bytes.Contains(body, []byte(compatCanary)) || !bytes.Contains(body, []byte(`"metadata":{"user_id":"synthetic-session-only"}`)) {
				t.Error("supported text leaked or service metadata changed")
			}
			for _, want := range []string{`"budget_tokens":31999`, `"display":"omitted"`, `"clear_thinking_20251015"`, `"keep":"all"`, `"run_synthetic"`, `"input_schema"`} {
				if !bytes.Contains(body, []byte(want)) {
					t.Errorf("control/tool structure lost: %s", want)
				}
			}
			if r.Header.Get("X-Api-Key") != "client-only-synthetic-key" || r.Header.Get("Anthropic-Version") != "2023-06-01" || r.Header.Get("Anthropic-Beta") != compatBeta {
				t.Error("selected credential/version/beta missing")
			}
			for _, name := range []string{"Cookie", "X-Forwarded-For", "Authorization"} {
				if r.Header.Get(name) != "" {
					t.Errorf("forbidden outbound header: %s", name)
				}
			}
			var request struct {
				System string `json:"system"`
			}
			if err := json.Unmarshal(body, &request); err != nil || request.System == "" || request.System == compatCanary {
				t.Error("missing masked request alias", err)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Set-Cookie", "upstream-synthetic-cookie="+compatCanary)
			_, _ = io.WriteString(w, compatSSE(request.System))
		}))
		defer up.Close()
		deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), up.URL, "anthropic", &localCalls, &legacyCalls)
		w := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages?beta=true", compatHTTPBody())
		if w.Code != http.StatusOK || upstreamCalls.Load() != 1 || localCalls.Load() != 0 || legacyCalls.Load() != 0 {
			t.Fatalf("mask first turn: status=%d upstream=%d local=%d legacy=%d", w.Code, upstreamCalls.Load(), localCalls.Load(), legacyCalls.Load())
		}
		for _, want := range []string{compatCanary, `"id":"toolu_synthetic"`, `"name":"run_synthetic"`, `"stop_reason":"tool_use"`, `"output_tokens":8`, "message_stop"} {
			if !strings.Contains(w.Body.String(), want) {
				t.Errorf("validated answer missing %s", want)
			}
		}
		if w.Header.Get("Set-Cookie") != "" || strings.Contains(w.Body.String(), "client-only-synthetic-key") {
			t.Fatal("upstream header or credential released")
		}
		state, _ := json.Marshal(deps.Runtime.State())
		if bytes.Contains(state, []byte(compatCanary)) || bytes.Contains(state, []byte("synthetic-session-only")) || bytes.Contains(state, []byte("synthetic-profile")) {
			t.Fatal("private body/session/profile retained in aggregate state")
		}
	})

	t.Run("controls-free-direct-preserves-selected-client-auth", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			auth  http.Header
			key   string
			value string
		}{
			{"api-key", http.Header{"Cookie": {"synthetic-cookie"}, "X-Forwarded-For": {"synthetic-client"}}, "X-Api-Key", "client-only-synthetic-key"},
			{"authorization", http.Header{"X-Api-Key": nil, "Authorization": {"Bearer client-only-synthetic-key"}, "X-Client-Id": {"synthetic-client"}}, "Authorization", "Bearer client-only-synthetic-key"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var localCalls, legacyCalls atomic.Int32
				seen := make(chan http.Header, 1)
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					seen <- r.Header.Clone()
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"content":[]}`)
				}))
				defer up.Close()
				endpoint, err := url.Parse(up.URL)
				if err != nil {
					t.Fatal(err)
				}
				rt := clientCompatRuntime(t, "mask")
				rt.clientControls = newClientControlTable(nil)
				deps := compatHTTPDeps(t, rt, up.URL, "anthropic", &localCalls, &legacyCalls)
				deps.Resolve = func([]byte) (HTTPRoute, error) {
					return HTTPRoute{Mode: "anthropic", Model: compatModel, Upstream: endpoint}, nil
				}
				plain := []byte(`{"model":"synthetic-supported","max_tokens":100,"messages":[{"role":"user","content":"plain"}]}`)
				result := compatHTTPCallWithHeaders(t, NewProtectedHTTP(deps), "/v1/messages", plain, tc.auth)
				if result.Code != http.StatusOK || localCalls.Load() != 0 || legacyCalls.Load() != 0 {
					t.Fatalf("controls-free direct route without a router-held key: %d", result.Code)
				}
				headers := compatWaitSignal(t, seen)
				if headers.Get(tc.key) != tc.value || headers.Get("Cookie") != "" || headers.Get("X-Forwarded-For") != "" || headers.Get("X-Client-Id") != "" {
					t.Fatal("selected direct credential missing or identifying header forwarded")
				}
				other := "Authorization"
				if tc.key == "Authorization" {
					other = "X-Api-Key"
				}
				if headers.Get(other) != "" {
					t.Fatal("unexpected second direct auth field forwarded")
				}
			})
		}
	})

	t.Run("direct-redirect-does-not-send-credential-to-second-origin", func(t *testing.T) {
		var first, second atomic.Int32
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			second.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"content":[]}`)
		}))
		defer other.Close()
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			first.Add(1)
			if r.Header.Get("X-Api-Key") != "client-only-synthetic-key" {
				t.Error("selected origin did not receive the chosen credential")
			}
			w.Header().Set("Location", other.URL+"/v1/messages")
			w.WriteHeader(http.StatusTemporaryRedirect)
		}))
		defer up.Close()
		var localCalls, legacyCalls atomic.Int32
		deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), up.URL, "anthropic", &localCalls, &legacyCalls)
		plain := []byte(`{"model":"synthetic-supported","max_tokens":100,"messages":[{"role":"user","content":"plain"}]}`)
		result := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages", plain)
		if result.Code != http.StatusBadGateway || first.Load() != 1 || second.Load() != 0 || localCalls.Load() != 0 || legacyCalls.Load() != 0 || strings.Contains(result.Body.String(), "client-only-synthetic-key") {
			t.Fatalf("direct redirect escaped configured origin: status=%d first=%d second=%d", result.Code, first.Load(), second.Load())
		}
	})

	t.Run("bad-response-never-releases-prefix", func(t *testing.T) {
		for _, suffix := range []string{
			"", // no message_stop after a valid text/tool prefix
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"" + compatCanary + "\"}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"redacted_thinking\",\"data\":\"opaque-synthetic\"}}\n\n",
		} {
			t.Run(fmt.Sprintf("suffix-%d", len(suffix)), func(t *testing.T) {
				var localCalls, legacyCalls atomic.Int32
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					prefix := strings.Split(compatSSE("safe"), "event: message_stop")[0]
					_, _ = io.WriteString(w, prefix+suffix)
				}))
				defer up.Close()
				deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), up.URL, "anthropic", &localCalls, &legacyCalls)
				w := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages?beta=true", compatHTTPBody())
				if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "event: content_block") || strings.Contains(w.Body.String(), "toolu_synthetic") || strings.Contains(w.Body.String(), compatCanary) {
					t.Fatalf("unverified prefix or raw error released: status=%d", w.Code)
				}
			})
		}
	})

	t.Run("modes-and-translated-guard", func(t *testing.T) {
		for _, mode := range []string{"mask", "detect", "bypass", "off"} {
			t.Run(mode, func(t *testing.T) {
				var upstreamCalls, localCalls, legacyCalls atomic.Int32
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upstreamCalls.Add(1)
					body, _ := io.ReadAll(r.Body)
					if mode == "detect" || mode == "bypass" {
						if !bytes.Contains(body, []byte(compatCanary)) {
							t.Error("unprotected direct mode changed original request")
						}
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"type":"message","content":[]}`)
				}))
				defer up.Close()
				rt := clientCompatRuntime(t, mode)
				if mode == "off" {
					rt = NewRuntime(t.TempDir()) // global-off never calls Policy.Prepare
				}
				deps := compatHTTPDeps(t, rt, up.URL, "anthropic", &localCalls, &legacyCalls)
				w := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages?beta=true", compatHTTPBody())
				if mode == "off" {
					if w.Code != 200 || legacyCalls.Load() != 1 || upstreamCalls.Load() != 0 {
						t.Fatal("global-off did not take legacy path")
					}
				} else if w.Code != 200 || upstreamCalls.Load() != 1 || legacyCalls.Load() != 0 {
					t.Fatalf("direct %s route failed: %d", mode, w.Code)
				}
				deps.Resolve = func([]byte) (HTTPRoute, error) {
					route := HTTPRoute{Mode: "openai", Model: compatModel, Local: func(w http.ResponseWriter, r *http.Request, body []byte, _ string) {
						localCalls.Add(1)
						attempt := FromRequest(r)
						if attempt == nil {
							t.Error("translated route lacks a request-scoped attempt")
							return
						}
						if _, err := attempt.Prepare(Target{Model: compatModel, Provider: "openai", Translated: true}, body); err != nil {
							t.Error("translated candidate rejected:", err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						if err := attempt.CheckControl("synthetic-selected"); err != nil {
							t.Error("controls-free candidate model rejected:", err)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"content":[]}`)
					}}
					return route, nil
				}
				w = compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages?beta=true", compatHTTPBody())
				if mode == "off" {
					if w.Code != 200 || legacyCalls.Load() != 2 || localCalls.Load() != 0 {
						t.Fatal("global-off entered protected translated guard")
					}
				} else {
					want := http.StatusOK
					if w.Code != want || localCalls.Load() != 1 || upstreamCalls.Load() != 1 {
						t.Fatalf("translated %s selected-profile guard: %d", mode, w.Code)
					}
				}
				if mode != "off" {
					plain := []byte(`{"model":"synthetic-supported","max_tokens":100,"messages":[{"role":"user","content":"plain"}]}`)
					w = compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages", plain)
					if w.Code != http.StatusOK || localCalls.Load() != 2 {
						t.Fatal("translated Messages without controls lost its old route")
					}
				}
			})
		}
	})

	t.Run("configured-local-route-kinds", func(t *testing.T) {
		for _, kind := range []string{"model", "pool"} {
			t.Run(kind, func(t *testing.T) {
				var localCalls, legacyCalls atomic.Int32
				deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), "http://synthetic.invalid", kind, &localCalls, &legacyCalls)
				deps.Resolve = func([]byte) (HTTPRoute, error) {
					return HTTPRoute{Mode: kind, Model: compatModel, Local: func(w http.ResponseWriter, r *http.Request, body []byte, _ string) {
						localCalls.Add(1)
						if _, err := FromRequest(r).Prepare(Target{Model: compatModel, Provider: "openai", Translated: true}, body); err != nil {
							t.Error(err)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"content":[]}`)
					}}, nil
				}
				plain := []byte(`{"model":"synthetic-supported","max_tokens":100,"messages":[{"role":"user","content":"plain"}]}`)
				result := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages", plain)
				if result.Code != http.StatusOK || localCalls.Load() != 1 || legacyCalls.Load() != 0 {
					t.Fatalf("configured %s route lost: %d", kind, result.Code)
				}
			})
		}
	})

	t.Run("local-no-exchange-sse-terminal", func(t *testing.T) {
		plain := []byte(`{"model":"synthetic-supported","max_tokens":100,"messages":[{"role":"user","content":"plain"}]}`)
		for _, mode := range []string{"detect", "bypass"} {
			for _, tc := range []struct {
				name, stream string
				wantStatus   int
			}{
				{"truncated", strings.Split(compatSSE("safe"), "event: message_stop")[0], http.StatusBadGateway},
				{"opaque", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\n" +
					"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"redacted_thinking\",\"data\":\"opaque-synthetic\"}}\n\n" +
					"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
					"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", http.StatusOK},
				{"terminal-suffix", compatSSE("safe") + "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"not-part-of-response\"}}\n\n", http.StatusOK},
			} {
				t.Run(mode+"/"+tc.name, func(t *testing.T) {
					var localCalls, legacyCalls atomic.Int32
					deps := compatHTTPDeps(t, clientCompatRuntime(t, mode), "http://synthetic.invalid", "model", &localCalls, &legacyCalls)
					deps.Resolve = func([]byte) (HTTPRoute, error) {
						return HTTPRoute{Mode: "model", Model: compatModel, Local: func(w http.ResponseWriter, r *http.Request, body []byte, _ string) {
							localCalls.Add(1)
							if _, err := FromRequest(r).Prepare(Target{Model: compatModel, Provider: "openai", Translated: true}, body); err != nil {
								t.Error(err)
								return
							}
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, tc.stream)
						}}, nil
					}
					result := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages", plain)
					if result.Code != tc.wantStatus || localCalls.Load() != 1 || strings.Contains(result.Body.String(), "not-part-of-response") || tc.wantStatus != http.StatusOK && strings.Contains(result.Body.String(), "event: content_block") {
						t.Fatalf("unverified local SSE released: mode=%s case=%s status=%d body=%q", mode, tc.name, result.Code, result.Body.String())
					}
					if tc.wantStatus == http.StatusOK && !strings.HasSuffix(result.Body.String(), "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n") {
						t.Fatal("validated local SSE lost its logical terminal")
					}
				})
			}
		}
	})

	t.Run("count-and-admission", func(t *testing.T) {
		var upstreamCalls, localCalls, legacyCalls atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamCalls.Add(1)
			wire, err := io.ReadAll(r.Body)
			if err != nil || bytes.Contains(wire, []byte(compatCanary)) {
				t.Error("count text not masked", err)
			}
			if r.URL.Path != "/v1/messages/count_tokens" || !bytes.Contains(wire, []byte(`"budget_tokens":31999`)) {
				t.Error("count route or controls changed")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"input_tokens":12}`)
		}))
		defer up.Close()
		deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), up.URL, "openai", &localCalls, &legacyCalls)
		deps.Resolve = func([]byte) (HTTPRoute, error) {
			return HTTPRoute{Mode: "openai", Model: compatModel, Local: func(w http.ResponseWriter, r *http.Request, body []byte, _ string) {
				if _, err := FromRequest(r).Prepare(Target{Model: compatModel, Provider: "openai", Translated: true, TokenCount: true}, body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				localCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": len(body) / 4})
			}}, nil
		}
		for i, body := range [][]byte{compatHTTPBody(), []byte(`{"model":"synthetic-supported","max_tokens":100,"messages":[{"role":"user","content":"plain"}]}`)} {
			w := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages/count_tokens", body)
			var count struct {
				InputTokens int `json:"input_tokens"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &count); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || count.InputTokens != len(body)/4 || upstreamCalls.Load() != 0 || localCalls.Load() != int32(i+1) {
				t.Fatalf("local count status=%d body=%s", w.Code, w.Body.String())
			}
		}
		endpoint, err := url.Parse(up.URL)
		if err != nil {
			t.Fatal(err)
		}
		deps.Resolve = func([]byte) (HTTPRoute, error) {
			return HTTPRoute{Mode: "anthropic", Model: compatModel, Upstream: endpoint}, nil
		}
		w := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages/count_tokens", compatHTTPBody())
		if w.Code != http.StatusOK || upstreamCalls.Load() != 1 || w.Body.String() != `{"input_tokens":12}` {
			t.Fatalf("direct count failed: %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("invalid-headers-and-config-before-network", func(t *testing.T) {
		var upstreamCalls, localCalls, legacyCalls atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { upstreamCalls.Add(1) }))
		defer up.Close()
		rt := clientCompatRuntime(t, "mask")
		deps := compatHTTPDeps(t, rt, up.URL, "anthropic", &localCalls, &legacyCalls)
		h := NewProtectedHTTP(deps)
		for _, extra := range []http.Header{
			{"Authorization": {"Bearer client-only-synthetic-key"}},
			{"X-Api-Key": {"first", "second"}},
			{"X-Api-Key": {" "}},
			{"X-Api-Key": nil},
		} {
			w := compatHTTPCallWithHeaders(t, h, "/v1/messages?beta=true", compatHTTPBody(), extra)
			if w.Code != http.StatusBadRequest || upstreamCalls.Load() != 0 {
				t.Fatal("mixed or malformed client authentication sent upstream")
			}
		}
		for _, path := range []string{"/v1/messages?beta=true&extra=1", "/v1/messages/"} {
			w := compatHTTPCall(t, h, path, compatHTTPBody())
			if w.Code != http.StatusBadRequest || upstreamCalls.Load() != 0 {
				t.Fatal("unsupported path/query reached provider")
			}
		}
		if err := os.WriteFile(filepath.Join(rt.home, "privacy-profiles.json"), []byte(`{"enabled":false,`), 0600); err != nil {
			t.Fatal(err)
		}
		w := compatHTTPCall(t, h, "/v1/messages?beta=true", compatHTTPBody())
		if w.Code != http.StatusServiceUnavailable || upstreamCalls.Load() != 0 || legacyCalls.Load() != 0 {
			t.Fatal("invalid config became global-off or reached network")
		}
	})

	t.Run("identifying-headers-rejected-or-stripped", func(t *testing.T) {
		var localCalls, legacyCalls atomic.Int32
		var leaked atomic.Bool
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Cookie") != "" || r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Client-Id") != "" {
				leaked.Store(true)
			}
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"content":[]}`)
		}))
		defer up.Close()
		deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), up.URL, "anthropic", &localCalls, &legacyCalls)
		w := compatHTTPCallWithHeaders(t, NewProtectedHTTP(deps), "/v1/messages?beta=true", compatHTTPBody(), http.Header{
			"Cookie": {"synthetic-cookie=" + compatCanary}, "X-Forwarded-For": {compatCanary}, "X-Client-Id": {compatCanary},
		})
		if leaked.Load() || w.Code != http.StatusOK && w.Code < 400 {
			t.Fatal("client-identifying header forwarded or ambiguous admission")
		}
	})
}

type compatClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*compatTimer
	armed  chan time.Duration
}

type compatTimer struct {
	clock   *compatClock
	at      time.Time
	fn      func()
	stopped bool
}

func newCompatClock() *compatClock {
	// Kernel read/write deadlines still use wall time; fake timer advances
	// begin at the current instant rather than an already expired epoch.
	return &compatClock{now: time.Now(), armed: make(chan time.Duration, 64)}
}

func (c *compatClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *compatClock) AfterFunc(d time.Duration, fn func()) LifecycleTimer {
	c.mu.Lock()
	timer := &compatTimer{clock: c, at: c.now.Add(d), fn: fn}
	c.timers = append(c.timers, timer)
	c.mu.Unlock()
	c.armed <- d
	return timer
}

func (timer *compatTimer) Stop() bool {
	timer.clock.mu.Lock()
	defer timer.clock.mu.Unlock()
	if timer.stopped {
		return false
	}
	timer.stopped = true
	return true
}

func (c *compatClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var callbacks []func()
	for _, timer := range c.timers {
		if !timer.stopped && !timer.at.After(c.now) {
			timer.stopped = true
			callbacks = append(callbacks, timer.fn)
		}
	}
	c.mu.Unlock()
	for _, fn := range callbacks {
		fn()
	}
}

func (c *compatClock) waitArm(t *testing.T, duration time.Duration) {
	t.Helper()
	for {
		select {
		case armed := <-c.armed:
			if armed == duration {
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("lifecycle timer %v was not armed", duration)
		}
	}
}

type compatAsyncResult struct {
	response *compatHTTPResult
	err      error
}

func compatStartCall(t *testing.T, handler http.Handler, path string, body []byte) (<-chan compatAsyncResult, context.CancelFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); server.Close() })
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Anthropic-Beta", compatBeta)
	r.Header.Set("X-Api-Key", "client-only-synthetic-key")
	ch := make(chan compatAsyncResult, 1)
	go func() {
		client := server.Client()
		client.Transport = &http.Transport{Proxy: nil}
		resp, err := client.Do(r)
		if err != nil {
			ch <- compatAsyncResult{err: err}
			return
		}
		defer resp.Body.Close()
		result := &compatHTTPResult{Code: resp.StatusCode, Header: resp.Header.Clone()}
		_, err = result.Body.ReadFrom(resp.Body)
		ch <- compatAsyncResult{response: result, err: err}
	}()
	return ch, cancel
}

func compatAwait(t *testing.T, ch <-chan compatAsyncResult) *compatHTTPResult {
	t.Helper()
	select {
	case result := <-ch:
		if result.err != nil || result.response == nil {
			t.Fatalf("synthetic HTTP call: %v", result.err)
		}
		return result.response
	case <-time.After(3 * time.Second):
		t.Fatal("synthetic HTTP call did not terminate")
		return nil
	}
}

type compatNotifyBody struct {
	io.ReadCloser
	reads chan<- int
}

func (b compatNotifyBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		select {
		case b.reads <- n:
		default:
		}
	}
	return n, err
}

type compatNotifyTransport struct {
	base    http.RoundTripper
	reads   chan<- int
	headers chan<- struct{}
}

func (tr compatNotifyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := tr.base.RoundTrip(r)
	if err == nil {
		resp.Body = compatNotifyBody{ReadCloser: resp.Body, reads: tr.reads}
		select {
		case tr.headers <- struct{}{}:
		default:
		}
	}
	return resp, err
}

func compatObservedClient(reads chan<- int, headers chan<- struct{}) *http.Client {
	return &http.Client{Transport: compatNotifyTransport{base: &http.Transport{Proxy: nil}, reads: reads, headers: headers}}
}

func compatWaitSignal[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("synthetic transport did not reach expected phase")
		var zero T
		return zero
	}
}

func compatWaitInactive(t *testing.T, rt *Runtime) {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for rt.State().Active != 0 {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("protected Exchanges/slots remained active after cancellation")
		}
	}
}

// Stage A only: the lifecycle implementation/clock interfaces do not exist on
// @45aae67. These tests specify behavior, not compiling RED or observed PASS.
func TestProtectedHTTPLifecycle(t *testing.T) {
	t.Run("unsupported-downstream-before-dispatch", func(t *testing.T) {
		var upstreamCalls, localCalls, legacyCalls atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { upstreamCalls.Add(1) }))
		defer up.Close()
		deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), up.URL, "anthropic", &localCalls, &legacyCalls)
		r := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", bytes.NewReader(compatHTTPBody()))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Anthropic-Beta", compatBeta)
		w := httptest.NewRecorder() // deliberately lacks an unwrappable socket
		NewProtectedHTTP(deps).ServeHTTP(w, r)
		if w.Code != http.StatusServiceUnavailable || upstreamCalls.Load() != 0 {
			t.Fatal("writer without SetWriteDeadline reached provider")
		}
	})

	t.Run("inbound-read-bound-before-dispatch", func(t *testing.T) {
		var upstreamCalls, localCalls, legacyCalls atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { upstreamCalls.Add(1) }))
		defer up.Close()
		deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), up.URL, "anthropic", &localCalls, &legacyCalls)
		deps.Limits.Inbound = 50 * time.Millisecond // test limit, not a production default
		server := httptest.NewServer(NewProtectedHTTP(deps))
		defer server.Close()
		addr := server.Listener.Addr().String()
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_, err = fmt.Fprintf(conn, "POST /v1/messages?beta=true HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n{", addr)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal("stalled inbound read did not terminate", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || upstreamCalls.Load() != 0 {
			t.Fatal("inbound timeout reached provider or returned success")
		}
	})

	t.Run("headers-and-idle-have-distinct-clocks", func(t *testing.T) {
		for _, phase := range []string{"headers", "ping-idle"} {
			t.Run(phase, func(t *testing.T) {
				var localCalls, legacyCalls atomic.Int32
				entered := make(chan struct{})
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					if phase == "ping-idle" {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
						w.(http.Flusher).Flush()
					}
					close(entered)
					<-r.Context().Done()
				}))
				defer up.Close()
				clock := newCompatClock()
				deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), up.URL, "anthropic", &localCalls, &legacyCalls)
				deps.Clock = clock
				deps.Limits = LifecycleLimits{Inbound: 200 * time.Millisecond, Headers: 300 * time.Millisecond, Idle: 100 * time.Millisecond, Total: 800 * time.Millisecond, Cleanup: 90 * time.Millisecond}
				done, _ := compatStartCall(t, NewProtectedHTTP(deps), "/v1/messages?beta=true", compatHTTPBody())
				compatWaitSignal(t, entered)
				if phase == "headers" {
					clock.waitArm(t, deps.Limits.Headers)
					clock.advance(deps.Limits.Headers + time.Millisecond)
				} else {
					clock.waitArm(t, deps.Limits.Idle)
					clock.advance(deps.Limits.Idle + time.Millisecond)
				}
				w := compatAwait(t, done)
				if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "event: ping") || strings.Contains(w.Body.String(), compatCanary) {
					t.Fatalf("%s timeout released unverified body: %d", phase, w.Code)
				}
			})
		}
	})

	t.Run("json-body-bytes-reset-idle-not-total", func(t *testing.T) {
		var localCalls, legacyCalls atomic.Int32
		continueBody := make(chan struct{})
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"type":"message","content":[`)
			w.(http.Flusher).Flush()
			<-continueBody
			_, _ = io.WriteString(w, `]}`)
		}))
		defer up.Close()
		reads, headers := make(chan int, 4), make(chan struct{}, 1)
		clock := newCompatClock()
		deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), up.URL, "anthropic", &localCalls, &legacyCalls)
		deps.Client = compatObservedClient(reads, headers)
		deps.Clock = clock
		deps.Limits = LifecycleLimits{Inbound: 200 * time.Millisecond, Headers: 300 * time.Millisecond, Idle: 100 * time.Millisecond, Total: 800 * time.Millisecond, Cleanup: 90 * time.Millisecond}
		done, _ := compatStartCall(t, NewProtectedHTTP(deps), "/v1/messages?beta=true", compatHTTPBody())
		compatWaitSignal(t, headers)
		compatWaitSignal(t, reads)
		clock.waitArm(t, deps.Limits.Idle)
		clock.advance(90 * time.Millisecond)
		close(continueBody)
		compatWaitSignal(t, reads)
		clock.waitArm(t, deps.Limits.Idle)
		clock.advance(90 * time.Millisecond)
		if w := compatAwait(t, done); w.Code != http.StatusOK {
			t.Fatal("JSON progress was treated as idle or incomplete object was accepted")
		}
	})

	t.Run("only-useful-sse-frame-resets-idle", func(t *testing.T) {
		var localCalls, legacyCalls atomic.Int32
		continueDelta, continueTerminal := make(chan struct{}), make(chan struct{})
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\n"+
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
				"event: ping\ndata: {\"type\":\"ping\"}\n\n")
			w.(http.Flusher).Flush()
			<-continueDelta
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"safe\"}}\n\n")
			w.(http.Flusher).Flush()
			<-continueTerminal
			_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		}))
		defer up.Close()
		clock := newCompatClock()
		deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), up.URL, "anthropic", &localCalls, &legacyCalls)
		deps.Clock = clock
		deps.Limits = LifecycleLimits{Inbound: 200 * time.Millisecond, Headers: 300 * time.Millisecond, Idle: 100 * time.Millisecond, Total: 800 * time.Millisecond, Cleanup: 90 * time.Millisecond}
		done, _ := compatStartCall(t, NewProtectedHTTP(deps), "/v1/messages?beta=true", compatHTTPBody())
		clock.waitArm(t, deps.Limits.Idle)
		clock.advance(90 * time.Millisecond)
		close(continueDelta)
		clock.waitArm(t, deps.Limits.Idle) // only the nonempty text_delta may rearm
		clock.advance(90 * time.Millisecond)
		close(continueTerminal)
		if w := compatAwait(t, done); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "message_stop") {
			t.Fatal("useful SSE progress did not preserve a complete message")
		}
	})

	t.Run("local-provider-headers-start-idle-before-generated-output", func(t *testing.T) {
		var localCalls, legacyCalls atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer up.Close()
		clock := newCompatClock()
		deps := compatHTTPDeps(t, clientCompatRuntime(t, "detect"), up.URL, "model", &localCalls, &legacyCalls)
		deps.Clock = clock
		deps.Limits = LifecycleLimits{Inbound: 200 * time.Millisecond, Headers: 300 * time.Millisecond, Idle: 100 * time.Millisecond, Total: 800 * time.Millisecond, Cleanup: 90 * time.Millisecond}
		deps.Resolve = func([]byte) (HTTPRoute, error) {
			return HTTPRoute{Mode: "model", Model: compatModel, Local: func(_ http.ResponseWriter, r *http.Request, body []byte, _ string) {
				localCalls.Add(1)
				if _, err := FromRequest(r).Prepare(Target{Model: compatModel, Provider: "openai", Translated: true}, body); err != nil {
					t.Error(err)
					return
				}
				request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, up.URL, bytes.NewReader(body))
				if err != nil {
					t.Error(err)
					return
				}
				client := &http.Client{Transport: &http.Transport{Proxy: nil}}
				response, err := client.Do(request)
				if err != nil {
					t.Error(err)
					return
				}
				ObserveProviderHeaders(r)
				defer response.Body.Close()
				<-r.Context().Done()
			}}, nil
		}
		plain := []byte(`{"model":"synthetic-supported","max_tokens":100,"messages":[{"role":"user","content":"plain"}]}`)
		done, cancel := compatStartCall(t, NewProtectedHTTP(deps), "/v1/messages", plain)
		defer cancel()
		clock.waitArm(t, deps.Limits.Idle)
		clock.advance(deps.Limits.Idle + time.Millisecond)
		if w := compatAwait(t, done); w.Code != http.StatusBadGateway || localCalls.Load() != 1 {
			t.Fatalf("local provider stalled without bounded idle: %d", w.Code)
		}
	})

	t.Run("generated-local-headers-cannot-restart-provider-idle", func(t *testing.T) {
		var localCalls, legacyCalls atomic.Int32
		clock := newCompatClock()
		providerHeaders, generate := make(chan struct{}), make(chan struct{})
		generated := make(chan struct{})
		deps := compatHTTPDeps(t, clientCompatRuntime(t, "detect"), "http://synthetic.invalid", "model", &localCalls, &legacyCalls)
		deps.Clock = clock
		deps.Limits = LifecycleLimits{Inbound: 200 * time.Millisecond, Headers: 300 * time.Millisecond, Idle: 100 * time.Millisecond, Total: 800 * time.Millisecond, Cleanup: 90 * time.Millisecond}
		deps.Resolve = func([]byte) (HTTPRoute, error) {
			return HTTPRoute{Mode: "model", Model: compatModel, Local: func(w http.ResponseWriter, r *http.Request, body []byte, _ string) {
				localCalls.Add(1)
				if _, err := FromRequest(r).Prepare(Target{Model: compatModel, Provider: "openai", Translated: true}, body); err != nil {
					t.Error(err)
					return
				}
				ObserveProviderHeaders(r)
				close(providerHeaders)
				select {
				case <-generate:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
				close(generated)
				<-r.Context().Done()
			}}, nil
		}
		plain := []byte(`{"model":"synthetic-supported","max_tokens":100,"messages":[{"role":"user","content":"plain"}]}`)
		done, cancel := compatStartCall(t, NewProtectedHTTP(deps), "/v1/messages", plain)
		defer cancel()
		compatWaitSignal(t, providerHeaders)
		clock.waitArm(t, deps.Limits.Idle)
		clock.advance(90 * time.Millisecond)
		close(generate)
		compatWaitSignal(t, generated)
		clock.advance(11 * time.Millisecond)
		if w := compatAwait(t, done); w.Code != http.StatusBadGateway || localCalls.Load() != 1 {
			t.Fatalf("generated headers prolonged provider idle: %d", w.Code)
		}
	})

	t.Run("late-local-headers-after-timeout-do-not-panic", func(t *testing.T) {
		var localCalls, legacyCalls atomic.Int32
		clock := newCompatClock()
		panicked := make(chan bool, 1)
		deps := compatHTTPDeps(t, clientCompatRuntime(t, "detect"), "http://synthetic.invalid", "model", &localCalls, &legacyCalls)
		deps.Clock = clock
		deps.Limits = LifecycleLimits{Inbound: 200 * time.Millisecond, Headers: 300 * time.Millisecond, Idle: 100 * time.Millisecond, Total: 800 * time.Millisecond, Cleanup: 90 * time.Millisecond}
		deps.Resolve = func([]byte) (HTTPRoute, error) {
			return HTTPRoute{Mode: "model", Model: compatModel, Local: func(w http.ResponseWriter, r *http.Request, body []byte, _ string) {
				localCalls.Add(1)
				defer func() { panicked <- recover() != nil }()
				if _, err := FromRequest(r).Prepare(Target{Model: compatModel, Provider: "openai", Translated: true}, body); err != nil {
					t.Error(err)
					return
				}
				<-r.Context().Done()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
			}}, nil
		}
		plain := []byte(`{"model":"synthetic-supported","max_tokens":100,"messages":[{"role":"user","content":"plain"}]}`)
		done, _ := compatStartCall(t, NewProtectedHTTP(deps), "/v1/messages", plain)
		clock.waitArm(t, deps.Limits.Headers)
		clock.advance(deps.Limits.Headers + time.Millisecond)
		if compatWaitSignal(t, panicked) {
			t.Fatal("local headers after expiration panicked")
		}
		if w := compatAwait(t, done); w.Code != http.StatusBadGateway || localCalls.Load() != 1 {
			t.Fatalf("late local headers escaped deadline: %d", w.Code)
		}
	})

	t.Run("local-ping-after-anthropic-headers-does-not-extend-idle", func(t *testing.T) {
		var localCalls, legacyCalls atomic.Int32
		clock := newCompatClock()
		deps := compatHTTPDeps(t, clientCompatRuntime(t, "detect"), "http://synthetic.invalid", "model", &localCalls, &legacyCalls)
		deps.Clock = clock
		deps.Limits = LifecycleLimits{Inbound: 200 * time.Millisecond, Headers: 300 * time.Millisecond, Idle: 100 * time.Millisecond, Total: 800 * time.Millisecond, Cleanup: 90 * time.Millisecond}
		deps.Resolve = func([]byte) (HTTPRoute, error) {
			return HTTPRoute{Mode: "model", Model: compatModel, Local: func(w http.ResponseWriter, r *http.Request, body []byte, _ string) {
				localCalls.Add(1)
				if _, err := FromRequest(r).Prepare(Target{Model: compatModel, Provider: "openai", Translated: true}, body); err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\n"+
					"event: ping\ndata: {\"type\":\"ping\"}\n\n")
				<-r.Context().Done()
			}}, nil
		}
		plain := []byte(`{"model":"synthetic-supported","max_tokens":100,"messages":[{"role":"user","content":"plain"}]}`)
		done, _ := compatStartCall(t, NewProtectedHTTP(deps), "/v1/messages", plain)
		clock.waitArm(t, deps.Limits.Idle)
		clock.advance(deps.Limits.Idle + time.Millisecond)
		w := compatAwait(t, done)
		if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "event: ping") || localCalls.Load() != 1 {
			t.Fatalf("local non-useful frames extended idle or escaped: %d", w.Code)
		}
	})

	t.Run("local-useful-anthropic-delta-rearms-idle", func(t *testing.T) {
		var localCalls, legacyCalls atomic.Int32
		clock := newCompatClock()
		continueDelta, continueTerminal := make(chan struct{}), make(chan struct{})
		deps := compatHTTPDeps(t, clientCompatRuntime(t, "detect"), "http://synthetic.invalid", "model", &localCalls, &legacyCalls)
		deps.Clock = clock
		deps.Limits = LifecycleLimits{Inbound: 200 * time.Millisecond, Headers: 300 * time.Millisecond, Idle: 100 * time.Millisecond, Total: 800 * time.Millisecond, Cleanup: 90 * time.Millisecond}
		deps.Resolve = func([]byte) (HTTPRoute, error) {
			return HTTPRoute{Mode: "model", Model: compatModel, Local: func(w http.ResponseWriter, r *http.Request, body []byte, _ string) {
				localCalls.Add(1)
				if _, err := FromRequest(r).Prepare(Target{Model: compatModel, Provider: "openai", Translated: true}, body); err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\n"+
					"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
				select {
				case <-continueDelta:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"safe\"}}\n\n")
				select {
				case <-continueTerminal:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
					"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}}, nil
		}
		plain := []byte(`{"model":"synthetic-supported","max_tokens":100,"messages":[{"role":"user","content":"plain"}]}`)
		done, _ := compatStartCall(t, NewProtectedHTTP(deps), "/v1/messages", plain)
		clock.waitArm(t, deps.Limits.Idle)
		clock.advance(90 * time.Millisecond)
		close(continueDelta)
		clock.waitArm(t, deps.Limits.Idle)
		clock.advance(90 * time.Millisecond)
		close(continueTerminal)
		if w := compatAwait(t, done); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "message_stop") || localCalls.Load() != 1 {
			t.Fatalf("local useful delta did not keep a validated message alive: %d", w.Code)
		}
	})

	t.Run("partial-tool-stall-releases-no-tool", func(t *testing.T) {
		var localCalls, legacyCalls atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			partial, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": `{"query":"`}})
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\n"+
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_synthetic\",\"name\":\"run_synthetic\",\"input\":{}}}\n\n"+
				"event: content_block_delta\ndata: "+string(partial)+"\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer up.Close()
		clock := newCompatClock()
		deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), up.URL, "anthropic", &localCalls, &legacyCalls)
		deps.Clock = clock
		deps.Limits = LifecycleLimits{Inbound: 200 * time.Millisecond, Headers: 300 * time.Millisecond, Idle: 100 * time.Millisecond, Total: 800 * time.Millisecond, Cleanup: 90 * time.Millisecond}
		done, _ := compatStartCall(t, NewProtectedHTTP(deps), "/v1/messages?beta=true", compatHTTPBody())
		clock.waitArm(t, deps.Limits.Idle)
		clock.advance(deps.Limits.Idle + time.Millisecond)
		w := compatAwait(t, done)
		if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "toolu_synthetic") || strings.Contains(w.Body.String(), "event: content_block") {
			t.Fatal("partial tool or success prefix released before terminal")
		}
	})

	t.Run("already-cancelled-before-dispatch", func(t *testing.T) {
		var upstreamCalls, localCalls, legacyCalls atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { upstreamCalls.Add(1) }))
		defer up.Close()
		deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), up.URL, "anthropic", &localCalls, &legacyCalls)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", bytes.NewReader(compatHTTPBody())).WithContext(ctx)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Anthropic-Beta", compatBeta)
		w := &compatPartialWriter{ResponseRecorder: httptest.NewRecorder()}
		NewProtectedHTTP(deps).ServeHTTP(w, r)
		if upstreamCalls.Load() != 0 || localCalls.Load() != 0 {
			t.Fatal("canceled request dispatched")
		}
	})
}

type compatChunkBody struct {
	mu     sync.Mutex
	parts  [][]byte
	index  int
	offset int
	reads  atomic.Int32
	closed chan struct{}
	once   sync.Once
}

func (b *compatChunkBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	b.mu.Lock()
	if b.index < len(b.parts) {
		n := copy(p, b.parts[b.index][b.offset:])
		b.offset += n
		if b.offset == len(b.parts[b.index]) {
			b.index++
			b.offset = 0
		}
		b.mu.Unlock()
		return n, nil
	}
	b.mu.Unlock()
	<-b.closed // deliberately no EOF until the protected handler closes the body
	return 0, io.EOF
}

func (b *compatChunkBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

type compatRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn compatRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestProtectedHTTPTerminalSegmentation(t *testing.T) {
	prefix := compatSSE("safe")
	suffix := "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"not-part-of-response\"}}\n\n"
	for _, tc := range []struct {
		name  string
		parts []string
		reads int32
	}{
		{"same-read", []string{prefix + suffix}, 1},
		{"next-read", []string{prefix, suffix}, 1},
		{"split-terminal", []string{prefix[:len(prefix)-3], prefix[len(prefix)-3:], suffix}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var localCalls, legacyCalls atomic.Int32
			body := &compatChunkBody{closed: make(chan struct{})}
			for _, part := range tc.parts {
				body.parts = append(body.parts, []byte(part))
			}
			deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), "http://synthetic.invalid", "anthropic", &localCalls, &legacyCalls)
			deps.Client = &http.Client{Transport: compatRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, Request: r}, nil
			})}
			w := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages?beta=true", compatHTTPBody())
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "message_stop") || strings.Contains(w.Body.String(), "not-part-of-response") {
				t.Fatalf("terminal depends on TCP split or released suffix: %d", w.Code)
			}
			if body.reads.Load() != tc.reads {
				t.Fatalf("read beyond terminal: %d reads, expected %d", body.reads.Load(), tc.reads)
			}
			select {
			case <-body.closed:
			default:
				t.Fatal("terminal returned before closing upstream body")
			}
		})
	}
	t.Run("terminal-near-output-limit-ignores-same-read-suffix", func(t *testing.T) {
		var localCalls, legacyCalls atomic.Int32
		alias := strings.Repeat("a", (TrafficOutputLimit-len(compatSSE(""))-200)/2)
		prefix := compatSSE(alias)
		suffix := strings.Repeat("X", TrafficOutputLimit-len(prefix)+1)
		if len(prefix) > TrafficOutputLimit || len(prefix)+len(suffix) <= TrafficOutputLimit {
			t.Fatal("test fixture does not straddle the output limit")
		}
		for _, parts := range [][]string{{prefix[:1], prefix[1:] + suffix}, {prefix[:1], prefix[1:], suffix}} {
			body := &compatChunkBody{closed: make(chan struct{})}
			for _, part := range parts {
				body.parts = append(body.parts, []byte(part))
			}
			deps := compatHTTPDeps(t, clientCompatRuntime(t, "detect"), "http://synthetic.invalid", "anthropic", &localCalls, &legacyCalls)
			deps.Client = &http.Client{Transport: compatRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, Request: r}, nil
			})}
			w := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages", []byte(`{"model":"synthetic-supported","max_tokens":100,"messages":[{"role":"user","content":"plain"}]}`))
			if w.Code != http.StatusOK || w.Body.Len() != len(prefix) || !strings.HasSuffix(w.Body.String(), "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n") {
				t.Fatalf("terminal near the bound depends on read segmentation: %d bytes=%d want=%d", w.Code, w.Body.Len(), len(prefix))
			}
		}
	})
	t.Run("local-terminal-near-output-limit-ignores-suffix", func(t *testing.T) {
		alias := strings.Repeat("a", (TrafficOutputLimit-len(compatSSE(""))-200)/2)
		prefix := compatSSE(alias)
		suffix := strings.Repeat("X", TrafficOutputLimit-len(prefix)+1)
		if len(prefix) > TrafficOutputLimit || len(prefix)+len(suffix) <= TrafficOutputLimit {
			t.Fatal("test fixture does not straddle the output limit")
		}
		for _, parts := range [][]string{{prefix[:1], prefix[1:] + suffix}, {prefix[:1], prefix[1:], suffix}} {
			var localCalls, legacyCalls atomic.Int32
			deps := compatHTTPDeps(t, clientCompatRuntime(t, "detect"), "http://synthetic.invalid", "model", &localCalls, &legacyCalls)
			deps.Resolve = func([]byte) (HTTPRoute, error) {
				return HTTPRoute{Mode: "model", Model: compatModel, Local: func(w http.ResponseWriter, r *http.Request, body []byte, _ string) {
					localCalls.Add(1)
					if _, err := FromRequest(r).Prepare(Target{Model: compatModel, Provider: "openai", Translated: true}, body); err != nil {
						t.Error(err)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					for _, part := range parts {
						if _, err := io.WriteString(w, part); err != nil {
							t.Errorf("valid terminal prefix rejected on local write: %v", err)
							return
						}
					}
				}}, nil
			}
			w := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages", []byte(`{"model":"synthetic-supported","max_tokens":100,"messages":[{"role":"user","content":"plain"}]}`))
			if w.Code != http.StatusOK || w.Body.Len() != len(prefix) || !bytes.HasSuffix(w.Body.Bytes(), []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")) || localCalls.Load() != 1 {
				t.Fatalf("local terminal near the bound depends on write segmentation: %d bytes=%d want=%d", w.Code, w.Body.Len(), len(prefix))
			}
		}
	})
}

type compatProbeWriter struct {
	http.ResponseWriter
	entered     chan struct{}
	exited      chan struct{}
	enteredOnce sync.Once
	exitedOnce  sync.Once
	status      atomic.Int32
}

func (w *compatProbeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *compatProbeWriter) WriteHeader(status int) {
	w.status.Store(int32(status))
	w.ResponseWriter.WriteHeader(status)
}
func (w *compatProbeWriter) Write(p []byte) (int, error) {
	w.enteredOnce.Do(func() { close(w.entered) })
	n, err := w.ResponseWriter.Write(p)
	w.exitedOnce.Do(func() { close(w.exited) })
	return n, err
}

func TestProtectedHTTPClientIO(t *testing.T) {
	t.Run("blocked-real-client-write-and-cleanup", func(t *testing.T) {
		var localCalls, legacyCalls atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			largeDelta, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": strings.Repeat("x", 12<<20)}})
			stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
				"event: content_block_delta\ndata: " + string(largeDelta) + "\n\n" +
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
			_, _ = io.WriteString(w, stream) // one validated message under 16 MiB
		}))
		defer up.Close()
		rt := clientCompatRuntime(t, "mask")
		deps := compatHTTPDeps(t, rt, up.URL, "anthropic", &localCalls, &legacyCalls)
		clock := newCompatClock()
		deps.Clock = clock
		probe := &compatProbeWriter{entered: make(chan struct{}), exited: make(chan struct{})}
		serverDone := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(serverDone)
			probe.ResponseWriter = w
			NewProtectedHTTP(deps).ServeHTTP(probe, r)
		}))
		defer server.Close()
		conn, err := net.Dial("tcp", server.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetReadBuffer(1024)
		}
		request := compatHTTPBody()
		_, err = fmt.Fprintf(conn, "POST /v1/messages?beta=true HTTP/1.1\r\nHost: synthetic.test\r\nContent-Type: application/json\r\nX-Api-Key: client-only-synthetic-key\r\nAnthropic-Beta: %s\r\nContent-Length: %d\r\n\r\n%s", compatBeta, len(request), request)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-probe.entered:
		case <-time.After(20 * time.Second):
			t.Fatal("validated response never reached client Write")
		}
		select {
		case <-probe.exited:
			t.Fatal("kernel accepted entire response; blocked Write not demonstrated")
		case <-time.After(100 * time.Millisecond):
		}
		clock.waitArm(t, deps.Limits.Total)
		clock.advance(deps.Limits.Total + time.Millisecond) // total interrupts the actual Write
		compatWaitSignal(t, probe.exited)
		compatWaitSignal(t, serverDone)
		if probe.status.Load() != http.StatusOK || rt.State().Active != 0 {
			t.Fatal("postcommit status changed or Exchange/slot retained after Write")
		}
	})

	t.Run("four-slots-release-only-after-cancel", func(t *testing.T) {
		var localCalls, legacyCalls, dispatched atomic.Int32
		entered := make(chan struct{}, 4)
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			dispatched.Add(1)
			entered <- struct{}{}
			<-r.Context().Done()
		}))
		defer up.Close()
		deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), up.URL, "anthropic", &localCalls, &legacyCalls)
		h := NewProtectedHTTP(deps)
		var pending []<-chan compatAsyncResult
		var cancels []context.CancelFunc
		for i := 0; i < 4; i++ {
			ch, cancel := compatStartCall(t, h, "/v1/messages?beta=true", compatHTTPBody())
			pending, cancels = append(pending, ch), append(cancels, cancel)
			compatWaitSignal(t, entered)
		}
		w := compatHTTPCall(t, h, "/v1/messages?beta=true", compatHTTPBody())
		if w.Code != http.StatusServiceUnavailable || dispatched.Load() != 4 {
			t.Fatal("fifth request was queued or dispatched instead of fast 503")
		}
		for _, cancel := range cancels {
			cancel()
		}
		for _, ch := range pending {
			select {
			case <-ch: // cancellation may close transport without an HTTP response
			case <-time.After(3 * time.Second):
				t.Fatal("cancel failed to finish protected work")
			}
		}
		compatWaitInactive(t, deps.Runtime)
		next, cancel := compatStartCall(t, h, "/v1/messages?beta=true", compatHTTPBody())
		compatWaitSignal(t, entered) // an actual fifth dispatch proves a freed slot
		cancel()
		compatWaitSignal(t, next)
	})

	t.Run("partial-postcommit-write-never-starts-second-json", func(t *testing.T) {
		var localCalls, legacyCalls atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, compatSSE("safe"))
		}))
		defer up.Close()
		deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), up.URL, "anthropic", &localCalls, &legacyCalls)
		w := &compatPartialWriter{ResponseRecorder: httptest.NewRecorder()}
		r := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", bytes.NewReader(compatHTTPBody()))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Anthropic-Beta", compatBeta)
		r.Header.Set("X-Api-Key", "client-only-synthetic-key")
		NewProtectedHTTP(deps).ServeHTTP(w, r)
		if w.Code != http.StatusOK || w.Body.Len() != 1 || strings.Contains(w.Body.String(), `"error"`) || strings.Contains(w.Body.String(), "event: error") {
			t.Fatal("partial committed SSE write changed status or appended error")
		}
	})

	t.Run("http2-early-peer-disconnect-releases-slot", func(t *testing.T) {
		var localCalls, legacyCalls atomic.Int32
		dispatched, upstreamDone, serverDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
		proto := make(chan int, 1)
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			close(dispatched)
			<-r.Context().Done()
			close(upstreamDone)
		}))
		defer up.Close()
		deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), up.URL, "anthropic", &localCalls, &legacyCalls)
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(serverDone)
			proto <- r.ProtoMajor
			NewProtectedHTTP(deps).ServeHTTP(w, r)
		}))
		server.EnableHTTP2 = true
		server.StartTLS()
		defer server.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/messages?beta=true", bytes.NewReader(compatHTTPBody()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Anthropic-Version", "2023-06-01")
		req.Header.Set("Anthropic-Beta", compatBeta)
		req.Header.Set("X-Api-Key", "client-only-synthetic-key")
		clientDone := make(chan error, 1)
		go func() {
			resp, err := server.Client().Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			clientDone <- err
		}()
		if got := compatWaitSignal(t, proto); got != 2 {
			t.Fatalf("expected HTTP/2 provider dispatch, got HTTP/%d", got)
		}
		compatWaitSignal(t, dispatched)
		cancel()
		deadline := time.NewTimer(deps.Limits.Cleanup)
		defer deadline.Stop()
		for _, done := range []<-chan struct{}{serverDone, upstreamDone} {
			select {
			case <-done:
			case <-deadline.C:
				t.Fatal("HTTP/2 early disconnect retained protected work beyond cleanup deadline")
			}
		}
		select {
		case <-clientDone:
		case <-deadline.C:
			t.Fatal("HTTP/2 client did not terminate after cancel")
		}
		if deps.Runtime.State().Active != 0 || localCalls.Load() != 0 || legacyCalls.Load() != 0 {
			t.Fatal("HTTP/2 disconnect retained Exchange or crossed into local/legacy route")
		}
	})

	t.Run("http1-tcp-peer-close-releases-slot", func(t *testing.T) {
		var localCalls, legacyCalls atomic.Int32
		dispatched, upstreamDone, serverDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			close(dispatched)
			<-r.Context().Done()
			close(upstreamDone)
		}))
		defer up.Close()
		deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), up.URL, "anthropic", &localCalls, &legacyCalls)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(serverDone)
			NewProtectedHTTP(deps).ServeHTTP(w, r)
		}))
		defer server.Close()
		conn, err := net.Dial("tcp", server.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		body := compatHTTPBody()
		if _, err := fmt.Fprintf(conn, "POST /v1/messages?beta=true HTTP/1.1\r\nHost: synthetic.test\r\nContent-Type: application/json\r\nX-Api-Key: client-only-synthetic-key\r\nAnthropic-Version: 2023-06-01\r\nAnthropic-Beta: %s\r\nContent-Length: %d\r\n\r\n%s", compatBeta, len(body), body); err != nil {
			t.Fatal(err)
		}
		compatWaitSignal(t, dispatched)
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		deadline := time.NewTimer(deps.Limits.Cleanup)
		defer deadline.Stop()
		for _, done := range []<-chan struct{}{serverDone, upstreamDone} {
			select {
			case <-done:
			case <-deadline.C:
				t.Fatal("HTTP/1.1 early disconnect retained protected work beyond cleanup deadline")
			}
		}
		if deps.Runtime.State().Active != 0 || localCalls.Load() != 0 || legacyCalls.Load() != 0 {
			t.Fatal("HTTP/1.1 disconnect retained Exchange or crossed into local/legacy route")
		}
	})
}

type compatPartialWriter struct{ *httptest.ResponseRecorder }

func (w *compatPartialWriter) SetReadDeadline(time.Time) error  { return nil }
func (w *compatPartialWriter) SetWriteDeadline(time.Time) error { return nil }
func (w *compatPartialWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, errors.New("synthetic partial write")
	}
	n, _ := w.ResponseRecorder.Write(p[:1])
	return n, errors.New("synthetic partial write")
}

// These Stage A fixtures use real loopback HTTP at the outer client and
// Codex Sender boundaries, but inject the local call site. They do not prove
// root local.go wiring; that requires the separate integrated-router test.
func compatCodexPlainBody() []byte {
	return []byte(`{"model":"synthetic-supported","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hello SyntheticPrivateName"}]}`)
}

func compatCodexUpstreamError(ctx context.Context, rawStatus int, code string, retry []string, decoded bool, override ...string) error {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		for _, value := range retry {
			w.Header().Add("Retry-After", value)
		}
		if decoded {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"synthetic\",\"error\":{\"code\":%q,\"message\":\"raw-%s\"}}}\n\n", code, compatCanary)
			return
		}
		w.Header().Set("X-Upstream-Error", "raw-"+compatCanary)
		w.Header().Set("Set-Cookie", "raw-"+compatCanary)
		w.WriteHeader(rawStatus)
		if len(override) > 0 {
			_, _ = io.WriteString(w, override[0])
			return
		}
		_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"raw-%s"}}`, code, compatCanary)
	}))
	defer up.Close()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	_, err := codex.Exchange(ctx, []byte(`{"model":"synthetic-codex","input":[{"type":"message","role":"user","content":"safe"}]}`),
		func(ctx context.Context, payload []byte, headers http.Header) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, up.URL, bytes.NewReader(payload))
			if err != nil {
				return nil, err
			}
			req.Header = headers.Clone()
			return client.Do(req)
		}, codex.Options{}, nil)
	return err
}

func compatCodexEnvelope(t *testing.T, result *compatHTTPResult, status int, kind string) string {
	t.Helper()
	if result.Code != status || !strings.HasPrefix(result.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("error status/content type: %d %q, want %d JSON", result.Code, result.Header.Get("Content-Type"), status)
	}
	var response struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &response); err != nil || response.Type != "error" || response.Error.Type != kind || response.Error.Message == "" {
		t.Fatalf("unsafe/incomplete Anthropic error envelope: %q (%v)", result.Body.String(), err)
	}
	for _, secret := range []string{compatCanary, "raw-", "partial-tool", "synthetic-provider-key", "synthetic-profile"} {
		if strings.Contains(result.Body.String(), secret) || strings.Contains(fmt.Sprint(result.Header), secret) {
			t.Fatalf("raw upstream/body/header reached client: %s", secret)
		}
	}
	return response.Error.Message
}

func TestProtectedHTTPCodex429Precommit(t *testing.T) {
	cases := []struct {
		name        string
		rawStatus   int
		code        string
		retry       []string
		untyped     bool
		unknownHTTP bool
		beforeHTTP  bool
		decoded     bool
		incomplete  bool
		rawBody     string
		wantHeader  string
	}{
		{name: "original429-transient", rawStatus: 429, code: "rate_limit_exceeded", retry: []string{"7"}, wantHeader: "7"},
		{name: "original429-empty-body", rawStatus: 429, code: "rate_limit_exceeded", retry: []string{"7"}, incomplete: true},
		{name: "original429-malformed-body", rawStatus: 429, code: "rate_limit_exceeded", retry: []string{"7"}, incomplete: true, rawBody: "not-json"},
		{name: "original429-truncated-body", rawStatus: 429, code: "rate_limit_exceeded", retry: []string{"7"}, incomplete: true, rawBody: `{"error":{"code":"rate_limit_exceeded"`},
		{name: "original429-25h-not-clamped", rawStatus: 429, code: "rate_limit_exceeded", retry: []string{"90000"}},
		{name: "original429-no-header", rawStatus: 429, code: "rate_limit_exceeded"},
		{name: "original429-overflow-header", rawStatus: 429, code: "rate_limit_exceeded", retry: []string{"9223372036854775808"}},
		{name: "original429-malformed-header", rawStatus: 429, code: "slow_down", retry: []string{"not-a-delay"}},
		{name: "original429-duplicate-header", rawStatus: 429, code: "slow_down", retry: []string{"7", "8"}},
		{name: "original429-quota", rawStatus: 429, code: "insufficient_quota", retry: []string{"7"}},
		{name: "original429-unknown-code", rawStatus: 429, code: "synthetic_unrecognized", retry: []string{"7"}},
		{name: "raw503-mapped429-rate", rawStatus: 503, code: "rate_limit_exceeded", retry: []string{"7"}},
		{name: "raw503-mapped429-quota", rawStatus: 503, code: "insufficient_quota", retry: []string{"7"}},
		{name: "synthetic-decoded502-mapped429", rawStatus: 200, code: "rate_limit_exceeded", decoded: true},
		{name: "unknown-original-status", code: "rate_limit_exceeded", unknownHTTP: true},
		{name: "untyped429", untyped: true},
		{name: "error-before-headers", beforeHTTP: true},
	}
	for _, tc := range cases {
		for _, mode := range []string{"mask", "off"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				var localCalls, legacyCalls atomic.Int32
				rt := clientCompatRuntime(t, "mask")
				if mode == "off" {
					rt = NewRuntime(t.TempDir())
				}
				deps := compatHTTPDeps(t, rt, "http://synthetic.invalid", "codex", &localCalls, &legacyCalls)
				failure := func(r *http.Request) (int, error) {
					if tc.beforeHTTP {
						return 502, errors.New("raw-before-headers-" + compatCanary)
					}
					if tc.untyped {
						return 429, errors.New("raw-" + compatCanary)
					}
					if tc.unknownHTTP {
						return 429, &codex.ProtocolError{Code: tc.code, Status: 429, RetryAfter: 7 * time.Second}
					}
					var err error
					if tc.incomplete {
						err = compatCodexUpstreamError(r.Context(), tc.rawStatus, tc.code, tc.retry, tc.decoded, tc.rawBody)
					} else {
						err = compatCodexUpstreamError(r.Context(), tc.rawStatus, tc.code, tc.retry, tc.decoded)
					}
					if err == nil {
						t.Error("synthetic Codex failure was accepted as success")
						return 502, errors.New("raw-" + compatCanary)
					}
					status := tc.rawStatus
					if tc.decoded || status == 503 {
						status = 429 // the existing code table maps this outward status
					}
					return status, err
				}
				preset := func(w http.ResponseWriter) {
					w.Header().Set("Retry-After", "777")
					w.Header()["rEtRy-AfTeR"] = []string{"999"}
					w.Header().Set("Content-Length", "31415")
				}
				deps.Resolve = func([]byte) (HTTPRoute, error) {
					return HTTPRoute{Mode: "codex", Model: compatModel, Local: func(w http.ResponseWriter, r *http.Request, _ []byte, _ string) {
						localCalls.Add(1)
						status, err := failure(r)
						_, _ = ScrubLocalFailure(r, err, "raw-"+compatCanary, status)
						preset(w)
						w.Header().Set("Set-Cookie", "raw-"+compatCanary)
						w.WriteHeader(status)
						_, _ = io.WriteString(w, "raw-partial-tool-"+compatCanary)
					}}, nil
				}
				deps.Legacy = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					legacyCalls.Add(1)
					status, err := failure(r)
					preset(w)
					anthropicerror.WriteHTTP(w, FailureForResponse(r, err, status))
				})
				result := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages?beta=true", compatCodexPlainBody())
				wantStatus, wantType := http.StatusTooManyRequests, "rate_limit_error"
				if tc.beforeHTTP {
					wantStatus, wantType = http.StatusBadGateway, "api_error"
				} else if mode == "mask" && (tc.rawStatus != 429 || tc.unknownHTTP || tc.untyped || tc.decoded || tc.incomplete) {
					wantStatus, wantType = http.StatusBadGateway, "api_error"
				} else if mode == "off" && (tc.rawStatus != 429 || tc.unknownHTTP || tc.untyped || tc.decoded || tc.incomplete) {
					wantType = "api_error"
				}
				message := compatCodexEnvelope(t, result, wantStatus, wantType)
				wantHeader := ""
				if tc.name == "original429-transient" {
					wantHeader = tc.wantHeader
				}
				if got := result.Header.Get("Retry-After"); got != wantHeader {
					t.Fatalf("public Retry-After = %q, want %q", got, wantHeader)
				}
				if tc.name == "original429-25h-not-clamped" && (strings.Contains(message, "24h") || strings.Contains(message, "86400") || strings.Contains(result.Body.String(), "86400")) {
					t.Fatal("25h header was clamped or a 24h recovery was promised")
				}
				lowerMessage := strings.ToLower(message)
				if tc.name == "original429-quota" && (!strings.Contains(lowerMessage, "reported") || !strings.Contains(lowerMessage, "limit")) {
					t.Fatal("quota message did not attribute the reported limit to upstream")
				}
				if (tc.code == "insufficient_quota" || tc.code == "synthetic_unrecognized") && (strings.Contains(lowerMessage, "temporary") || strings.Contains(lowerMessage, "will reset")) {
					t.Fatal("quota/unknown error promised a transient recovery")
				}
				if tc.name == "original429-unknown-code" && strings.Contains(lowerMessage, "quota") {
					t.Fatal("unknown original 429 was classified as quota")
				}
				if mode == "mask" && (localCalls.Load() != 1 || legacyCalls.Load() != 0) || mode == "off" && (legacyCalls.Load() != 1 || localCalls.Load() != 0) {
					t.Fatal("protected/off path crossed into the other route")
				}
			})
		}
	}
}

func TestProtectedHTTPCodex429LatestCandidateWins(t *testing.T) {
	var localCalls, legacyCalls atomic.Int32
	deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), "http://synthetic.invalid", "codex", &localCalls, &legacyCalls)
	deps.Resolve = func([]byte) (HTTPRoute, error) {
		return HTTPRoute{Mode: "model", Model: compatModel, Local: func(w http.ResponseWriter, r *http.Request, body []byte, _ string) {
			localCalls.Add(1)
			if _, err := FromRequest(r).Prepare(Target{Model: compatModel, Provider: "codex", Translated: true, Protocol: "codex"}, body); err != nil {
				t.Error("first candidate rejected:", err)
				return
			}
			first := compatCodexUpstreamError(r.Context(), 429, "rate_limit_exceeded", []string{"7"}, false)
			if first == nil {
				t.Error("missing first candidate failure")
				return
			}
			_, _ = ScrubLocalFailure(r, first, "raw-first-"+compatCanary, 429)
			if _, err := FromRequest(r).Prepare(Target{Model: compatModel, Provider: "codex", Translated: true, Protocol: "codex"}, body); err != nil {
				t.Error("last candidate rejected:", err)
				return
			}
			last := compatCodexUpstreamError(r.Context(), 429, "insufficient_quota", []string{"11"}, false)
			if last == nil {
				t.Error("missing second candidate failure")
				return
			}
			_, _ = ScrubLocalFailure(r, last, "raw-last-"+compatCanary, 429)
			w.Header().Set("Retry-After", "7") // stale transient header from first candidate
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, "raw-last-"+compatCanary)
		}}, nil
	}
	result := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages?beta=true", compatCodexPlainBody())
	message := compatCodexEnvelope(t, result, http.StatusTooManyRequests, "rate_limit_error")
	if !strings.Contains(strings.ToLower(message), "reported") || result.Header.Get("Retry-After") != "" || localCalls.Load() != 1 {
		t.Fatal("first candidate's transient state or header replaced the final quota outcome")
	}
}

func TestProtectedHTTPCodex429ValidationBeatsFailure(t *testing.T) {
	for _, tc := range []struct{ name, contentType, body string }{
		{"incomplete-json", "application/json", `{"type":"message","content":[` + compatCanary},
		{"incomplete-tool-sse", "text/event-stream", strings.Split(compatSSE("safe"), "event: message_stop")[0] +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"partial-tool\",\"name\":\"run_synthetic\",\"input\":{}}\n\n"},
		{"complete-after-failed-candidate", "text/event-stream", compatSSE("safe")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var localCalls, legacyCalls atomic.Int32
			deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), "http://synthetic.invalid", "codex", &localCalls, &legacyCalls)
			deps.Resolve = func([]byte) (HTTPRoute, error) {
				return HTTPRoute{Mode: "codex", Model: compatModel, Local: func(w http.ResponseWriter, r *http.Request, _ []byte, _ string) {
					localCalls.Add(1)
					if _, err := FromRequest(r).Prepare(Target{Model: compatModel, Provider: "codex", Translated: true}, compatCodexPlainBody()); err != nil {
						t.Error("candidate preparation failed:", err)
						return
					}
					err := compatCodexUpstreamError(r.Context(), 429, "rate_limit_exceeded", []string{"7"}, false)
					if err == nil {
						t.Error("expected synthetic original HTTP 429")
						return
					}
					_, _ = ScrubLocalFailure(r, err, "raw-"+compatCanary, 429)
					w.Header().Set("Content-Type", tc.contentType)
					w.Header().Set("Retry-After", "7")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, tc.body)
				}}, nil
			}
			result := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages?beta=true", compatCodexPlainBody())
			compatCodexEnvelope(t, result, http.StatusBadGateway, "api_error")
			if result.Header.Get("Retry-After") != "" || localCalls.Load() != 1 {
				t.Fatal("original 429 overrode failed Restore/validation or leaked header")
			}
		})
	}
}

func TestProtectedHTTPCodex429DeadlineBeatsFailure(t *testing.T) {
	var localCalls, legacyCalls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	clock := newCompatClock()
	deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), "http://synthetic.invalid", "codex", &localCalls, &legacyCalls)
	deps.Clock = clock
	deps.Limits = LifecycleLimits{Inbound: 200 * time.Millisecond, Headers: 300 * time.Millisecond, Idle: 100 * time.Millisecond, Total: 800 * time.Millisecond, Cleanup: 90 * time.Millisecond}
	deps.Resolve = func([]byte) (HTTPRoute, error) {
		return HTTPRoute{Mode: "codex", Model: compatModel, Local: func(w http.ResponseWriter, r *http.Request, _ []byte, _ string) {
			localCalls.Add(1)
			err := compatCodexUpstreamError(r.Context(), 429, "rate_limit_exceeded", []string{"7"}, false)
			if err == nil {
				t.Error("expected synthetic original HTTP 429")
				return
			}
			_, _ = ScrubLocalFailure(r, err, "raw-"+compatCanary, 429)
			close(entered)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}}, nil
	}
	done, _ := compatStartCall(t, NewProtectedHTTP(deps), "/v1/messages?beta=true", compatCodexPlainBody())
	compatWaitSignal(t, entered)
	clock.waitArm(t, deps.Limits.Total)
	clock.advance(deps.Limits.Total + time.Millisecond)
	result := compatAwait(t, done)
	compatCodexEnvelope(t, result, http.StatusBadGateway, "api_error")
	if result.Header.Get("Retry-After") != "" || localCalls.Load() != 1 {
		t.Fatal("deadline changed protected refusal into Codex 429")
	}
}

// Committed delivery is intentionally a different oracle from the precommit
// HTTP matrix: after any byte is written the handler may only close or append
// a whole safe SSE error frame, never change status, replay or write JSON.
// The complete-error-frame path still needs a B-approved postcommit injection
// seam; a Local callback alone cannot force a client commit through the buffer.
func TestProtectedHTTPCodex429CommittedPartialWrite(t *testing.T) {
	var localCalls, legacyCalls atomic.Int32
	deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), "http://synthetic.invalid", "codex", &localCalls, &legacyCalls)
	deps.Resolve = func([]byte) (HTTPRoute, error) {
		return HTTPRoute{Mode: "codex", Model: compatModel, Local: func(w http.ResponseWriter, r *http.Request, body []byte, _ string) {
			localCalls.Add(1)
			attempt := FromRequest(r)
			if attempt == nil {
				t.Error("missing protected candidate attempt")
				return
			}
			if _, err := attempt.Prepare(Target{Model: compatModel, Provider: "codex", Translated: true}, body); err != nil {
				t.Error("candidate prepare failed:", err)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, compatSSE("safe"))
		}}, nil
	}
	writer := &compatPartialWriter{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", bytes.NewReader(compatCodexPlainBody()))
	r.Header.Set("Content-Type", "application/json")
	NewProtectedHTTP(deps).ServeHTTP(writer, r)
	if writer.Code != http.StatusOK || writer.Body.Len() != 1 || strings.Contains(writer.Body.String(), "event: error") || strings.Contains(writer.Body.String(), `"error"`) || localCalls.Load() != 1 {
		t.Fatal("partial committed SSE write got a second JSON/error frame or changed status")
	}
}

func TestProtectedHTTPCodex429OtherAdapterCannotBorrowLocalException(t *testing.T) {
	var localCalls, legacyCalls atomic.Int32
	deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), "http://synthetic.invalid", "openai", &localCalls, &legacyCalls)
	deps.Resolve = func([]byte) (HTTPRoute, error) {
		return HTTPRoute{Mode: "openai", Model: compatModel, Local: func(w http.ResponseWriter, r *http.Request, _ []byte, _ string) {
			localCalls.Add(1)
			err := compatCodexUpstreamError(r.Context(), 429, "rate_limit_exceeded", []string{"7"}, false)
			if err == nil {
				t.Error("missing synthetic provider failure")
				return
			}
			_, _ = ScrubLocalFailure(r, err, "raw-"+compatCanary, 429)
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, "raw-"+compatCanary)
		}}, nil
	}
	result := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages?beta=true", compatCodexPlainBody())
	compatCodexEnvelope(t, result, http.StatusBadGateway, "api_error")
	if result.Header.Get("Retry-After") != "" || localCalls.Load() != 1 {
		t.Fatal("generic OpenAI adapter inherited the Codex-local exception")
	}
}

func TestProtectedHTTPDirect429PreservesStatusWithoutRawBody(t *testing.T) {
	var localCalls, legacyCalls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Retry-After", "7")
		w.Header().Set("Set-Cookie", "raw-"+compatCanary)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"raw-`+compatCanary+`"}}`)
	}))
	defer up.Close()
	deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), up.URL, "anthropic", &localCalls, &legacyCalls)
	result := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages?beta=true", compatHTTPBody())
	compatCodexEnvelope(t, result, http.StatusTooManyRequests, "rate_limit_error")
	if result.Header.Get("Retry-After") != "7" || result.Header.Get("Set-Cookie") != "" || localCalls.Load() != 0 || legacyCalls.Load() != 0 {
		t.Fatal("direct 429 lost safe retry metadata or escaped its route")
	}
}
