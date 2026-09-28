package privacy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDetectHTTPPreservesProviderResponses(t *testing.T) {
	for _, tc := range []struct {
		name, media, body string
		status            int
	}{
		{"stream error", "text/event-stream", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"provider busy\"}}\n\n", 200},
		{"new stream event", "text/event-stream", "event: new_provider_event\ndata: {\"type\":\"new_provider_event\"}\n\n", 200},
		{"provider failure", "application/json", `{"error":{"message":"provider busy"}}`, 529},
		{"non-json failure", "text/plain", "provider temporarily unavailable", 503},
		{"redirect", "text/plain", "provider redirect", 307},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.media)
				w.Header().Set("Retry-After", "3")
				w.Header().Set("Location", "/other-provider-endpoint")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer up.Close()
			rt := clientCompatRuntime(t, "detect")
			var local, legacy atomic.Int32
			deps := compatHTTPDeps(t, rt, up.URL, "anthropic", &local, &legacy)
			// Exercise the handler directly so the test client does not follow redirects.
			request := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", bytes.NewReader(compatHTTPBody()))
			request.Header.Set("Content-Type", "application/json")
			got := httptest.NewRecorder()
			NewProtectedHTTP(deps).ServeHTTP(got, request)
			if got.Code != tc.status || got.Body.String() != tc.body || got.Header().Get("Retry-After") != "3" || got.Header().Get("Location") != "/other-provider-endpoint" {
				t.Fatalf("provider response changed: status=%d body=%s", got.Code, got.Body)
			}
			if rt.State().Detected != 1 || rt.State().Findings[KindOrg] == 0 || legacy.Load() != 0 {
				t.Fatal("detection did not run, or raw history path was used")
			}
		})
	}
}

func TestDetectHTTPStreamsBeforeProviderFinishes(t *testing.T) {
	for _, route := range []string{"anthropic", "model"} {
		t.Run(route, func(t *testing.T) {
			const first = "event: ping\ndata: {\"type\":\"ping\"}\n\n"
			release := make(chan struct{})
			stream := func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, first)
				_ = http.NewResponseController(w).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}
			up := httptest.NewServer(http.HandlerFunc(stream))
			defer up.Close()
			rt := clientCompatRuntime(t, "detect")
			if _, err := rt.Snapshot(); err != nil {
				t.Fatal(err)
			}
			var local, legacy atomic.Int32
			deps := compatHTTPDeps(t, rt, up.URL, route, &local, &legacy)
			resolve := deps.Resolve
			deps.Resolve = func(body []byte) (HTTPRoute, error) {
				r, err := resolve(body)
				r.Local = func(w http.ResponseWriter, req *http.Request, body []byte, _ string) {
					if _, err := FromRequest(req).Prepare(Target{Model: compatModel}, body); err != nil {
						t.Error(err)
						return
					}
					stream(w, req)
				}
				return r, err
			}
			server := httptest.NewServer(NewProtectedHTTP(deps))
			defer server.Close()
			defer close(release)
			client := server.Client()
			client.Timeout = time.Second
			req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", bytes.NewReader(compatHTTPBody()))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Api-Key", "synthetic")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("first event blocked behind complete response: %v", err)
			}
			defer resp.Body.Close()
			got := make([]byte, len(first))
			if _, err := io.ReadFull(resp.Body, got); err != nil || string(got) != first {
				t.Fatalf("first event changed: %q %v", got, err)
			}
		})
	}
}

func TestDetectFailureDoesNotRejectRequest(t *testing.T) {
	rt := clientCompatRuntime(t, "detect")
	policy, err := rt.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	// Valid JSON, beyond the detector's bounded inspection budget.
	body := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", TrafficInputLimit) + `"}]}`)
	x, wire, err := policy.Prepare(Target{Model: compatModel}, body)
	if err != nil || x != nil || !bytes.Equal(wire, body) {
		t.Fatalf("detector failure rejected or changed request: %v", err)
	}
	if rt.State().DetectionErrors != 1 || rt.State().Rejected != 0 {
		t.Fatal("analysis failure was not counted separately from rejected traffic")
	}
}

func TestDetectTransportDoesNotBypassMaskingProfile(t *testing.T) {
	rt := clientCompatRuntime(t, "detect")
	config := `{"version":1,"enabled":true,"default":"detect","profiles":[{"id":"detect","name":"Detect","enabled":true,"mode":"detect","rules":{}},{"id":"mask","name":"Mask","enabled":true,"mode":"mask","rules":{"entries":[{"kind":"org","forms":["` + compatCanary + `"]}]}}],"bindings":[{"kind":"provider","target":"anthropic","profile":"mask"}]}`
	if err := os.WriteFile(filepath.Join(rt.home, "privacy-profiles.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(compatCanary)) {
			t.Error("detect transport bypassed the selected masking profile")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":[]}`)
	}))
	defer up.Close()
	var local, legacy atomic.Int32
	deps := compatHTTPDeps(t, rt, up.URL, "anthropic", &local, &legacy)
	result := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages", compatHTTPBody())
	if result.Code != http.StatusOK || rt.State().Protected != 1 || rt.State().Detected != 0 || legacy.Load() != 0 {
		t.Fatal("mixed policy did not use masking")
	}
}
