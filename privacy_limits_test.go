package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"localrouter/internal/history"
)

func TestPrivacyAnthropicLimits(t *testing.T) {
	for _, mode := range []string{"off", "mask", "detect"} {
		for _, tc := range []struct {
			name    string
			path    string
			status  int
			headers bool
			local   bool
			want    string
		}{
			{"message", "/v1/messages?beta=true", 200, true, false, "fresh"},
			{"stream", "/v1/messages?beta=true", 200, true, false, "fresh"},
			{"rate limit", "/v1/messages", 429, true, false, "fresh"},
			{"no headers", "/v1/messages", 200, false, false, "no_headers"},
			{"error without headers", "/v1/messages", 529, false, false, "unavailable"},
			{"token count", "/v1/messages/count_tokens", 200, true, false, "unavailable"},
			{"local provider", "/v1/messages", 200, true, true, "unavailable"},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if tc.name == "stream" {
						w.Header().Set("Content-Type", "text/event-stream")
					}
					w.Header().Set("X-Private", trafficCanary)
					if tc.headers {
						w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
					}
					w.WriteHeader(tc.status)
					switch {
					case tc.name == "stream":
						_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"type\":\"message\",\"content\":[]}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					case tc.status >= 400:
						_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"limited"}}`)
					case r.URL.Path == "/v1/messages/count_tokens":
						_, _ = io.WriteString(w, `{"input_tokens":12}`)
					case tc.local:
						_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
					default:
						_, _ = io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"ok"}]}`)
					}
				}))
				defer up.Close()
				home := t.TempDir()
				base, _ := url.Parse(up.URL)
				cfg := config{upstream: base, firstByte: time.Second, local: oneProvider(up.URL, "good")}
				route := modelRoute{Mode: "anthropic"}
				if tc.local {
					route = modelRoute{Mode: "model", Model: "p/good"}
				}
				cfg.local.Routes = map[string]map[string]modelRoute{"test": {"default": route}}
				cs := newConfigStore(cfg, filepath.Join(home, "providers.json"))
				privacyMode := mode
				if mode == "off" {
					privacyMode = "mask"
				}
				policy := fmt.Sprintf(`{"version":1,"enabled":%t,"profiles":[{"id":"p","name":"P","mode":%q,"rules":{}}],"bindings":[]}`, mode != "off", privacyMode)
				if err := os.WriteFile(filepath.Join(home, "privacy-profiles.json"), []byte(policy), 0600); err != nil {
					t.Fatal(err)
				}
				st, hl := history.New(10, ""), newHealth("")
				u := newUIServer(st, cs, hl)
				body := trafficBody
				if tc.name == "stream" {
					body = strings.TrimSuffix(body, "}") + `,"stream":true}`
				}
				response := trafficCall(newMainHandler(cfg, cs, st, hl, u), tc.path, body)
				if response.Code != tc.status {
					t.Fatalf("status=%d, want %d: %s", response.Code, tc.status, response.Body)
				}
				view := u.limits.View(time.Now())
				if view.State != tc.want {
					t.Fatalf("limits state=%s, want %s", view.State, tc.want)
				}
				if tc.want == "fresh" && (len(view.Windows) != 1 || view.Windows[0].UsedPercent == nil || *view.Windows[0].UsedPercent != 42) {
					t.Fatalf("limits not captured: %+v", view)
				}
				if mode != "off" && (len(st.List()) != 0 || response.Header().Get("X-Private") != "") {
					t.Fatal("private response metadata leaked")
				}
			})
		}
	}
}
