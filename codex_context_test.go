package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	conf "localrouter/internal/config"
	"localrouter/internal/history"
)

func TestCodexRestoresMissingToolCallFromSameSession(t *testing.T) {
	hist := history.New(10, "")
	previous := []byte(strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call-1","name":"Read","input":{}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"file.txt\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
	}, "\n\n"))
	hist.Add(&history.Record{Start: time.Now().Add(-time.Second), End: time.Now(), Session: "session-a", Status: 200,
		Resp: history.ParseResponse("text/event-stream", "", previous, false)})
	input := openaiRequest{Model: "gpt-test", Messages: []openaiMsg{
		{Role: "tool", ToolCallID: "call-1", Content: "file contents"},
		{Role: "user", Content: "continue"},
	}}

	repaired, err := restoreCodexCalls(input, "session-a", hist)
	if err != nil {
		t.Fatal(err)
	}
	out, err := toCodex(repaired)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Input) != 3 {
		t.Fatalf("expected restored call, original output and user message; got %d items", len(out.Input))
	}
	call := out.Input[0].(map[string]any)
	result := out.Input[1].(map[string]any)
	var args struct {
		Path string `json:"path"`
	}
	argsJSON, _ := call["arguments"].(string)
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		t.Fatalf("invalid restored tool arguments: %v", err)
	}
	if call["type"] != "function_call" || call["call_id"] != "call-1" || call["name"] != "Read" || args.Path != "file.txt" ||
		result["type"] != "function_call_output" || result["call_id"] != "call-1" || result["output"] != "file contents" {
		t.Fatalf("tool call and result were not paired: call=%v result type=%v", call["type"], result["type"])
	}
	if out.Input[2].(map[string]any)["role"] != "user" {
		t.Fatal("following user message moved or lost")
	}
}

func TestCodexMissingCallNeverLeaksOtherSession(t *testing.T) {
	hist := history.New(10, "")
	hist.Add(&history.Record{Start: time.Now().Add(-time.Second), End: time.Now(), Session: "session-b", Status: 200,
		Resp: &history.Response{Blocks: []history.Block{{Type: "tool_use", ID: "call-1", Name: "Read", Input: `{}`}}}})
	input := openaiRequest{Messages: []openaiMsg{{Role: "tool", ToolCallID: "call-1", Content: "private"}}}
	if _, err := restoreCodexCalls(input, "session-a", hist); err == nil {
		t.Fatal("borrowed another session's tool call")
	}
	if _, err := restoreCodexCalls(input, "", hist); err == nil {
		t.Fatal("borrowed a tool call without a session identity")
	}
}

func TestCodexDoesNotRestoreCallFromErroredStream(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call-1","name":"Read","input":{}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"file.txt\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"error","error":{"type":"api_error","message":"stream interrupted"}}`,
	}, "\n\n")
	hist := history.New(10, "")
	hist.Add(&history.Record{Start: time.Now().Add(-time.Second), End: time.Now(), Session: "session-a", Status: 200,
		Resp: history.ParseResponse("text/event-stream", "", []byte(stream), false)})
	input := openaiRequest{Messages: []openaiMsg{{Role: "tool", ToolCallID: "call-1", Content: "private result"}}}
	if _, err := restoreCodexCalls(input, "session-a", hist); err == nil {
		t.Fatal("restored a tool call from a failed stream")
	}
}

func TestCodexRestoresParallelToolResults(t *testing.T) {
	hist := history.New(10, "")
	hist.Add(&history.Record{Start: time.Now().Add(-time.Second), End: time.Now(), Session: "session-a", Status: 200,
		Resp: &history.Response{Blocks: []history.Block{
			{Type: "tool_use", ID: "call-1", Name: "Read", Input: `{"path":"a"}`},
			{Type: "tool_use", ID: "call-2", Name: "Read", Input: `{"path":"b"}`},
		}}})
	input := openaiRequest{Messages: []openaiMsg{
		{Role: "tool", ToolCallID: "call-2", Content: "b result"},
		{Role: "tool", ToolCallID: "call-1", Content: "a result"},
	}}
	repaired, err := restoreCodexCalls(input, "session-a", hist)
	if err != nil {
		t.Fatal(err)
	}
	out, err := toCodex(repaired)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Input) != 4 {
		t.Fatalf("expected two tool call/output pairs, got %d", len(out.Input))
	}
	for i, id := range []string{"call-2", "call-1"} {
		call, result := out.Input[2*i].(map[string]any), out.Input[2*i+1].(map[string]any)
		if call["type"] != "function_call" || call["call_id"] != id || result["type"] != "function_call_output" || result["call_id"] != id {
			t.Fatalf("tool output %d was not paired with its original call", i)
		}
	}
}

func TestCodexOrphanedResultIsStoppedBeforeUpstream(t *testing.T) {
	oldAuth, oldTransport := codexAuth, http.DefaultTransport
	defer func() { codexAuth = oldAuth; http.DefaultTransport = oldTransport }()
	codexAuth = &codexAuthStore{loaded: true, credential: usageCredential("account")}
	upstreamCalls := 0
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		upstreamCalls++
		return usageResponse(400, "upstream rejected orphaned result"), nil
	})
	cfg := config{Local: localSetup{Providers: []provider{{Name: "codex", Type: "codex", BaseURL: conf.CodexBaseURL}},
		Models: []localModel{{Provider: "codex", Model: "gpt-test"}}, Preferred: "codex/gpt-test"}, FirstByte: time.Second}
	body := []byte(`{"model":"claude-opus-5-5","stream":true,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"private result"}]}]}`)
	w := httptest.NewRecorder()
	handleLocal(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(body))), cfg, body, nil, newHealth(""), history.New(10, ""))
	if w.Code != http.StatusBadRequest || upstreamCalls != 0 || strings.Contains(w.Body.String(), "private result") {
		t.Fatalf("orphaned output leaked to upstream or error response (status=%d calls=%d)", w.Code, upstreamCalls)
	}
}

func TestCodexRestoredCallStreamsWithoutAnotherError(t *testing.T) {
	hist := history.New(10, "")
	previous := &history.Record{Start: time.Now().Add(-time.Second), End: time.Now(), Session: "session-a", Status: 200,
		Resp: &history.Response{Blocks: []history.Block{{Type: "tool_use", ID: "call-1", Name: "Read", Input: `{"path":"file.txt"}`}}}}
	hist.Add(previous)
	input := openaiRequest{Model: "gpt-test", Stream: true, Messages: []openaiMsg{
		{Role: "tool", ToolCallID: "call-1", Content: "file contents"},
	}}
	repaired, err := restoreCodexCalls(input, "session-a", hist)
	if err != nil {
		t.Fatal(err)
	}
	out, err := toCodex(repaired)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Input []struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		} `json:"input"`
	}
	if err := json.Unmarshal(payload, &got); err != nil || len(got.Input) != 2 || got.Input[0].Type != "function_call" || got.Input[1].Type != "function_call_output" || got.Input[0].CallID != got.Input[1].CallID {
		t.Fatal("invalid Codex input on streaming route")
	}
	oldAuth, oldTransport := codexAuth, http.DefaultTransport
	defer func() { codexAuth = oldAuth; http.DefaultTransport = oldTransport }()
	codexAuth = &codexAuthStore{loaded: true, credential: usageCredential("account")}
	upstreamCalls := 0
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		upstreamCalls++
		var actual codexRequest
		if err := json.NewDecoder(r.Body).Decode(&actual); err != nil {
			t.Error(err)
		}
		if len(actual.Input) != 2 || actual.Input[0].(map[string]any)["type"] != "function_call" || actual.Input[1].(map[string]any)["type"] != "function_call_output" {
			return usageResponse(400, "missing function call"), nil
		}
		stream := strings.Join([]string{
			`data: {"type":"response.output_text.delta","delta":"done"}`,
			`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":1}}}`,
		}, "\n\n")
		return usageResponse(200, stream), nil
	})
	cfg := config{Local: localSetup{Providers: []provider{{Name: "codex", Type: "codex", BaseURL: conf.CodexBaseURL}},
		Models: []localModel{{Provider: "codex", Model: "gpt-test"}}, Preferred: "codex/gpt-test"}, FirstByte: time.Second}
	body := []byte(`{"model":"claude-opus-5-5","stream":true,"metadata":{"user_id":"{\"session_id\":\"session-a\"}"},"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"file contents"}]}]}`)
	w := httptest.NewRecorder()
	handleLocal(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(body))), cfg, body, nil, newHealth(""), hist)
	if w.Code != http.StatusOK || upstreamCalls != 1 || !strings.Contains(w.Body.String(), `"text":"done"`) || !strings.Contains(w.Body.String(), "event: message_stop") {
		t.Fatalf("streaming route did not complete (status=%d calls=%d)", w.Code, upstreamCalls)
	}
}
