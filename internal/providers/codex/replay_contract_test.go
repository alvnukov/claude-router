package codex

import (
	"encoding/json"
	"strings"
	"testing"
)

const contractUser = `{"type":"message","role":"user","content":"Find router"}`
const contractAssistant = `{"type":"message","role":"assistant","content":"Checking."}`
const contractVisibleCall = `{"type":"function_call","call_id":"call-1","name":"search","arguments":"{\"a\":1,\"b\":2}"}`
const contractToolResult = `{"type":"function_call_output","call_id":"call-1","output":"found"}`

func contractRequest(items ...string) []byte {
	return []byte(`{"model":"codex-model","prompt_cache_key":"cache-key","reasoning":{"effort":"high"},"include":["reasoning.encrypted_content"],"input":[` + strings.Join(items, ",") + `]}`)
}

func contractReplayFixture(t *testing.T) (json.RawMessage, Completion) {
	t.Helper()
	result, err := Read(strings.NewReader(contractSSE(
		contractItemEvent(0, `{"type":"reasoning","id":"rs-1","summary":[],"encrypted_content":"opaque+/signed==","extra":{"number":9007199254740993}}`),
		contractItemEvent(1, `{"type":"message","id":"msg-1","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Checking."}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}}`),
		contractItemEvent(2, `{"type":"function_call","id":"fc-1","call_id":"call-1","name":"search","arguments":"{ \"b\" : 2, \"a\": 1 }","encrypted_function_args":["opaque-tool-state"]}`),
		contractCompleted("resp-tool", `,"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":5}}`),
	)), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result.TurnState = "sticky-turn-1"
	state, err := Capture("account-A/model-A", contractRequest(contractUser), result)
	if err != nil {
		t.Fatal(err)
	}
	return state, result
}

func contractInputs(t *testing.T, payload []byte) []json.RawMessage {
	t.Helper()
	var request struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatal(err)
	}
	return request.Input
}

func TestReplaySurvivesRestartAndToolArgumentCanonicalization(t *testing.T) {
	state, result := contractReplayFixture(t)
	// Persist and load through JSON to exercise the actual restart boundary.
	persisted, err := json.Marshal(map[string][]json.RawMessage{"records": {state}})
	if err != nil {
		t.Fatal(err)
	}
	var loaded struct {
		Records []json.RawMessage `json:"records"`
	}
	if err := json.Unmarshal(persisted, &loaded); err != nil {
		t.Fatal(err)
	}
	payload := contractRequest(contractUser, contractAssistant, contractVisibleCall, contractToolResult)
	restored, turnState, err := Restore("account-A/model-A", payload, loaded.Records)
	if err != nil || turnState != "sticky-turn-1" {
		t.Fatalf("turn state=%q err=%v", turnState, err)
	}
	input := contractInputs(t, restored)
	if len(input) != 5 {
		t.Fatalf("restored input has %d items, want 5", len(input))
	}
	contractJSONEqual(t, input[0], []byte(contractUser))
	for i, native := range result.Output {
		contractJSONEqual(t, input[i+1], native)
	}
	contractJSONEqual(t, input[4], []byte(contractToolResult))
	var request map[string]json.RawMessage
	if err := json.Unmarshal(restored, &request); err != nil {
		t.Fatal(err)
	}
	contractJSONEqual(t, request["reasoning"], []byte(`{"effort":"high"}`))
	contractJSONEqual(t, request["prompt_cache_key"], []byte(`"cache-key"`))
}

func TestReplayDoesNotImportStateAcrossAccountsModelsOrEditedBranches(t *testing.T) {
	state, _ := contractReplayFixture(t)
	for _, tt := range []struct {
		name, scope string
		input       []string
	}{
		{"other account", "account-B/model-A", []string{contractUser, contractAssistant, contractVisibleCall, contractToolResult}},
		{"other model", "account-A/model-B", []string{contractUser, contractAssistant, contractVisibleCall, contractToolResult}},
		{"unknown scope", "", []string{contractUser, contractAssistant, contractVisibleCall, contractToolResult}},
		{"edited earlier user", "account-A/model-A", []string{strings.Replace(contractUser, "Find router", "Find something else", 1), contractAssistant, contractVisibleCall, contractToolResult}},
		{"edited assistant", "account-A/model-A", []string{contractUser, strings.Replace(contractAssistant, "Checking.", "Changed.", 1), contractVisibleCall, contractToolResult}},
		{"edited arguments", "account-A/model-A", []string{contractUser, contractAssistant, strings.Replace(contractVisibleCall, `\"a\":1`, `\"a\":9`, 1), contractToolResult}},
		{"truncated branch", "account-A/model-A", []string{contractUser, contractAssistant}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			payload := contractRequest(tt.input...)
			restored, turnState, err := Restore(tt.scope, payload, []json.RawMessage{state})
			if err != nil || turnState != "" {
				t.Fatalf("turn state=%q err=%v", turnState, err)
			}
			contractJSONEqual(t, restored, payload)
		})
	}
}

func TestReplayPreservesHistoryButResetsTurnStateAtNextUser(t *testing.T) {
	state, _ := contractReplayFixture(t)
	payload := contractRequest(contractUser, contractAssistant, contractVisibleCall, contractToolResult,
		`{"type":"message","role":"user","content":"New task"}`)
	restored, turnState, err := Restore("account-A/model-A", payload, []json.RawMessage{state})
	if err != nil || turnState != "" {
		t.Fatalf("previous turn token leaked: state=%q err=%v", turnState, err)
	}
	input := contractInputs(t, restored)
	if len(input) != 6 || !strings.Contains(string(input[1]), "opaque+/signed==") {
		t.Fatalf("compatible reasoning history was lost: %s", restored)
	}
}

func TestReplayIgnoresCorruptAndFutureRecords(t *testing.T) {
	state, _ := contractReplayFixture(t)
	var future map[string]json.RawMessage
	if err := json.Unmarshal(state, &future); err != nil {
		t.Fatal(err)
	}
	future["version"] = json.RawMessage("999")
	futureJSON, err := json.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}
	payload := contractRequest(contractUser, contractAssistant, contractVisibleCall, contractToolResult)
	restored, turnState, err := Restore("account-A/model-A", payload, []json.RawMessage{json.RawMessage("{"), futureJSON})
	if err != nil || turnState != "" {
		t.Fatalf("corrupt state=%q err=%v", turnState, err)
	}
	contractJSONEqual(t, restored, payload)
}

func TestReplayDoesNotMatchToolArgumentsWithTrailingJSON(t *testing.T) {
	state, _ := contractReplayFixture(t)
	changed := `{"type":"function_call","call_id":"call-1","name":"search","arguments":"{\"a\":1,\"b\":2} {\"a\":9}"}`
	payload := contractRequest(contractUser, contractAssistant, changed, contractToolResult)
	restored, turnState, err := Restore("account-A/model-A", payload, []json.RawMessage{state})
	if err != nil {
		return // Explicit rejection is also safe; this is not equivalent history.
	}
	if turnState != "" {
		t.Fatalf("malformed edited arguments imported another prefix's turn state: %q", turnState)
	}
	contractJSONEqual(t, restored, payload)
}
