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

// Claude Code's beta SDK uses this exact query on both Messages endpoints.
func TestPrivacyTrafficClaudeBetaQuery(t *testing.T) {
	for _, mode := range []string{"mask", "detect"} {
		for _, local := range []bool{false, true} {
			for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
				name := mode + "/" + map[bool]string{false: "anthropic", true: "openai"}[local] + path
				t.Run(name, func(t *testing.T) {
					var calls atomic.Int32
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						wantQuery := "beta=true"
						if local {
							wantQuery = ""
						}
						if r.URL.RawQuery != wantQuery {
							t.Errorf("query = %q, want %q", r.URL.RawQuery, wantQuery)
						}
						body, _ := io.ReadAll(r.Body)
						var payload map[string]json.RawMessage
						if err := json.Unmarshal(body, &payload); err != nil {
							t.Error(err)
						}
						if strings.Contains(string(payload["messages"]), trafficCanary) != (mode == "detect") {
							t.Errorf("wrong protection mode %s", mode)
						}
						w.Header().Set("Content-Type", "application/json")
						switch {
						case strings.HasSuffix(path, "count_tokens"):
							_, _ = io.WriteString(w, `{"input_tokens":12}`)
						case local:
							_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
						default:
							_, _ = io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"ok"}]}`)
						}
					}))
					defer up.Close()
					h, history, home := trafficFixture(t, up, local)
					if mode == "detect" {
						file := filepath.Join(home, "privacy-profiles.json")
						b, err := os.ReadFile(file)
						if err != nil {
							t.Fatal(err)
						}
						b = bytes.Replace(b, []byte(`"rules":`), []byte(`"mode":"detect","rules":`), 1)
						if err := os.WriteFile(file, b, 0600); err != nil {
							t.Fatal(err)
						}
					}
					body := trafficBody
					if mode == "detect" {
						// Detection preserves content and uses the normal route translator.
						body = strings.TrimSuffix(body, "}") + `,"thinking":{"budget_tokens":31999,"type":"enabled","display":"omitted"},"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}}`
					}
					response := trafficCall(h, path+"?beta=true", body)
					wantStatus, wantCalls := http.StatusOK, int32(1)
					if local && path == "/v1/messages/count_tokens" {
						wantCalls = 0
					}
					if response.Code != wantStatus || calls.Load() != wantCalls {
						t.Fatalf("status=%d calls=%d, want %d/%d; response=%s", response.Code, calls.Load(), wantStatus, wantCalls, response.Body)
					}
					if len(history.List()) != 0 {
						t.Fatal("privacy request recorded in history")
					}
				})
			}
		}
	}
}

func TestPrivacyTrafficRejectsNoncanonicalBetaQueries(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer up.Close()
	h, history, _ := trafficFixture(t, up, false)
	for _, query := range []string{
		"beta=" + trafficCanary, "beta=false", "beta=true&secret=" + trafficCanary,
		"beta=true&beta=true", "beta=true&", "beta=true;secret=" + trafficCanary,
		"%62eta=true", "beta=%74rue", "beta=true%00", "beta=true%26secret=x",
	} {
		for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
			response := trafficCall(h, path+"?"+query, trafficBody)
			if response.Code != 400 || strings.Contains(response.Body.String(), trafficCanary) {
				t.Fatalf("unsafe query response: status=%d body=%s", response.Code, response.Body)
			}
		}
	}
	if calls.Load() != 0 || len(history.List()) != 0 {
		t.Fatal("unsafe query escaped privacy guard")
	}
}
