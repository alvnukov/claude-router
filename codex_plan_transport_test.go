package main

import (
	"bytes"
	"encoding/json"
	"io"
	conf "localrouter/internal/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatGPTPlanTransportPublicToolRoundTrip(t *testing.T) {
	useTestCodexHome(t, "https://auth.openai.com", http.DefaultClient)
	if err := codexAuth.save(planFixture(t, "https://auth.openai.com", true, time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	calls := 0
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://api.openai.com/v1/responses" || r.Header.Get("Authorization") != "Bearer opaque-old" {
			t.Errorf("wrong public request %s", r.URL)
		}
		for _, key := range []string{"ChatGPT-Account-Id", "originator", "session-id", "thread-id", "x-codex-turn-state", "x-client-request-id"} {
			if r.Header.Get(key) != "" {
				t.Errorf("private header %s sent", key)
			}
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["tools"].([]any)[0].(map[string]any)["type"] != "namespace" {
			t.Error("flat tools on public route")
		}
		if calls == 1 {
			return usageResponse(200, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call-1\",\"namespace\":\"functions\",\"name\":\"lookup\",\"arguments\":\"{}\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\"}}\n\n"), nil
		}
		items := body["input"].([]any)
		foundCall, foundOutput := false, false
		for _, raw := range items {
			item := raw.(map[string]any)
			if item["type"] == "function_call" {
				foundCall = item["call_id"] == "call-1" && item["namespace"] == "functions"
			}
			if item["type"] == "function_call_output" {
				foundOutput = item["call_id"] == "call-1"
			}
		}
		if !foundCall || !foundOutput {
			t.Error("tool round trip lost IDs or namespace")
		}
		return usageResponse(200, codexStreamOK), nil
	})
	cand := candidate{Key: "codex/gpt-6.1-sol", Provider: provider{Name: "codex", Type: "codex", BaseURL: conf.CodexBaseURL}}
	for i, input := range []string{`[{"type":"message","role":"user","content":"look up"}]`, `[{"type":"message","role":"user","content":"look up"},{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call-1","output":"done"}]`} {
		body := []byte(`{"model":"gpt-6.1-sol","instructions":"Be useful","input":` + input + `,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"lookup"},"store":false,"stream":true}`)
		a := tryCodexModel(httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body)), config{}, cand, body, false, "session")
		if a.err != nil {
			t.Fatal(a.err)
		}
		raw, err := io.ReadAll(a.resp.Body)
		a.resp.Body.Close()
		a.cancel()
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && (!strings.Contains(string(raw), "lookup") || !strings.Contains(string(raw), "call-1")) {
			t.Fatal("tool projection lost caller names")
		}
		if i == 1 && !strings.Contains(string(raw), "ok") {
			t.Fatal("tool output continuation failed")
		}
	}
	if calls != 2 {
		t.Fatalf("%d calls, expected caller-owned tool loop", calls)
	}
}
