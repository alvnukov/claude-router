package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"localrouter/internal/history"
	codexprovider "localrouter/internal/providers/codex"
)

func TestCodexNativeReplaySurvivesRouterRestart(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, change := range []string{"same", "cleared-history", "account", "session", "prefix"} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, change), func(t *testing.T) {
				seedTwoConnections(t)
				cfg := twoCodexPool()
				cfg.failover = false
				path := filepath.Join(t.TempDir(), "history.jsonl")
				hist := history.New(20, path)
				calls := 0
				old := http.DefaultTransport
				t.Cleanup(func() { http.DefaultTransport = old })
				http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					var request struct {
						Input []map[string]any `json:"input"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Fatal(err)
					}
					if calls == 1 {
						response := usageResponse(200, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_1\",\"encrypted_content\":\"opaque-private\"}}\n\n"+
							"data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"phase\":\"commentary\",\"content\":[{\"type\":\"output_text\",\"text\":\"Reading\"}]}}\n\n"+
							"data: {\"type\":\"response.output_item.done\",\"output_index\":2,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"Read\",\"arguments\":\"{\\\"path\\\":\\\"a\\\"}\"}}\n\n"+
							"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"end_turn\":false,\"usage\":{\"input_tokens\":100,\"input_tokens_details\":{\"cached_tokens\":80},\"output_tokens\":7}}}\n\n")
						response.Header.Set("x-codex-turn-state", "sticky")
						return response, nil
					}
					found := false
					for _, item := range request.Input {
						if item["encrypted_content"] == "opaque-private" {
							found = true
						}
						if item["id"] == "msg_1" && item["phase"] != "commentary" {
							t.Error("lost message phase")
						}
					}
					replayExpected := change == "same" || change == "cleared-history"
					if found != replayExpected {
						t.Errorf("opaque replay=%v, context change=%s", found, change)
					}
					wantState := ""
					if replayExpected {
						wantState = "sticky"
					}
					if got := r.Header.Get("x-codex-turn-state"); got != wantState {
						t.Errorf("turn state=%q, want %q", got, wantState)
					}
					last := request.Input[len(request.Input)-1]
					output, ok := last["output"].([]any)
					if !ok || len(output) != 2 || output[1].(map[string]any)["type"] != "input_image" {
						t.Error("structured tool image lost")
					}
					return usageResponse(200, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Done\"}]}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\",\"end_turn\":true}}\n\n"), nil
				})
				session := "session-a"
				user := map[string]any{"role": "user", "content": "hi"}
				run := func(messages []any) *history.Record {
					uid, _ := json.Marshal(map[string]string{"session_id": session})
					body, _ := json.Marshal(map[string]any{"model": "local-model", "stream": stream, "metadata": map[string]string{"user_id": string(uid)}, "messages": messages, "tools": []any{map[string]any{"name": "Read", "input_schema": map[string]any{"type": "object"}}}})
					record := &history.Record{Start: time.Now(), Session: session, ReqBody: body}
					hist.Add(record)
					writer := history.NewRecorder(httptest.NewRecorder(), 1<<20)
					trace := &history.Trace{}
					handleLocal(writer, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body))), cfg, body, trace, newHealth(""), hist)
					hist.Finish(record.ID, writer, trace)
					result := hist.Get(record.ID)
					if result.Failed() {
						t.Fatalf("request failed: status=%d error=%s", result.Status, result.Resp.Error)
					}
					if strings.Contains(string(result.RespBytes), "opaque-private") {
						t.Fatal("private state leaked to client")
					}
					return result
				}
				first := run([]any{user})
				var state codexprovider.Replay
				if json.Unmarshal(first.ProviderState, &state) != nil || len(state.Output) != 3 || !state.UsageKnown {
					t.Fatal("native state or usage not captured")
				}
				hist = history.New(20, path)
				if len(hist.List()) != 1 || len(hist.List()[0].ProviderState) == 0 {
					t.Fatal("native state did not survive restart")
				}
				switch change {
				case "cleared-history":
					hist.Clear()
				case "account":
					store, err := codexStoreFor(provider{Name: "codex", Type: "codex"})
					if err != nil {
						t.Fatal(err)
					}
					credential, _ := json.Marshal(usageCredential("different-account"))
					if err := os.WriteFile(store.cliPath, credential, 0600); err != nil {
						t.Fatal(err)
					}
					if err := store.importFromCLI(); err != nil {
						t.Fatal(err)
					}
				case "session":
					session = "session-b"
				case "prefix":
					user["content"] = "edited"
				}
				assistant := map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "Reading"}, map[string]any{"type": "tool_use", "id": "call_1", "name": "Read", "input": map[string]string{"path": "a"}}}}
				tool := map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": []any{map[string]string{"type": "text", "text": "file"}, map[string]any{"type": "image", "source": map[string]string{"type": "base64", "media_type": "image/png", "data": "aGVsbG8="}}}}}}
				second := run([]any{user, assistant, tool})
				if calls != 2 || second.Resp.StopReason != "end_turn" {
					t.Fatalf("unexpected calls=%d or stop=%q", calls, second.Resp.StopReason)
				}
			})
		}
	}
}

func TestCodexRequestPreservesParallelPolicyAndNumericEffort(t *testing.T) {
	var request anthropicRequest
	if err := json.Unmarshal([]byte(`{"tool_choice":{"type":"any","disable_parallel_tool_use":true},"messages":[{"role":"user","content":"hi"}]}`), &request); err != nil {
		t.Fatal(err)
	}
	chat, err := toOpenAI(request, "gpt")
	if err != nil {
		t.Fatal(err)
	}
	chat.ReasoningEffort = "1234"
	native, err := toCodex(chat)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(native)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	_ = json.Unmarshal(payload, &wire)
	if wire["parallel_tool_calls"] != false || wire["tool_choice"] != "required" || wire["instructions"] != "" || wire["reasoning"].(map[string]any)["effort"] != float64(1234) {
		t.Fatalf("wrong wire request: %s", payload)
	}
}
