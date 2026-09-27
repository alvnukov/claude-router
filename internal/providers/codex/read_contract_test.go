package codex

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func contractSSE(events ...string) string {
	return "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
}

func contractCompleted(id, extra string) string {
	return `{"type":"response.completed","response":{"id":"` + id + `","status":"completed"` + extra + `}}`
}

func contractItemEvent(index int, item string) string {
	data, _ := json.Marshal(map[string]any{"type": "response.output_item.done", "output_index": index, "item": json.RawMessage(item)})
	return string(data)
}

func contractJSONEqual(t *testing.T, actual, expected []byte) {
	t.Helper()
	var actualValue, expectedValue any
	for _, input := range []struct {
		data  []byte
		value *any
	}{{actual, &actualValue}, {expected, &expectedValue}} {
		decoder := json.NewDecoder(strings.NewReader(string(input.data)))
		decoder.UseNumber()
		if err := decoder.Decode(input.value); err != nil {
			t.Fatalf("invalid JSON %s: %v", input.data, err)
		}
	}
	if !reflect.DeepEqual(actualValue, expectedValue) {
		t.Fatalf("JSON mismatch\ngot  %s\nwant %s", actual, expected)
	}
}

func TestReadCompletedTextIsNeverLostOrDuplicated(t *testing.T) {
	message := `{"type":"message","id":"msg-1","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Hello world"}]}`
	for _, tt := range []struct {
		name   string
		events []string
	}{
		{"item done without delta", []string{contractItemEvent(0, message), contractCompleted("resp-1", "")}},
		{"completion output only", []string{contractCompleted("resp-1", `,"output":[`+message+`]`)}},
		{"partial delta plus done", []string{`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hello "}`, contractItemEvent(0, message), contractCompleted("resp-1", "")}},
		{"duplicate done and completion output", []string{contractItemEvent(0, message), contractItemEvent(0, message), contractCompleted("resp-1", `,"output":[`+message+`]`)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var emitted strings.Builder
			result, err := Read(strings.NewReader(contractSSE(tt.events...)), func(delta Delta) error {
				emitted.WriteString(delta.Content)
				return nil
			}, nil)
			if err != nil || result.Text != "Hello world" || emitted.String() != result.Text {
				t.Fatalf("text %q, emitted %q, error %v", result.Text, emitted.String(), err)
			}
			if len(result.Output) != 1 {
				t.Fatalf("native output count = %d, want 1", len(result.Output))
			}
			contractJSONEqual(t, result.Output[0], []byte(message))
		})
	}
}

func TestReadSSEMultilineCRLFCommentsAndNoTrailingDelimiter(t *testing.T) {
	wire := ": keepalive\r\nevent: ignored-label\r\nid: 42\r\n" +
		"data: {\"type\":\"response.output_item.done\",\r\n" +
		"data: \"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"m\",\"role\":\"assistant\",\"content\":[{\"type\":\"refusal\",\"refusal\":\"Cannot help\"}]}}\r\n\r\n" +
		"data: " + contractCompleted("resp-refusal", "")
	result, err := Read(strings.NewReader(wire), nil, nil)
	if err != nil || result.Text != "Cannot help" {
		t.Fatalf("multiline refusal = %q, %v", result.Text, err)
	}
}

func TestReadRefusalDeltaReconcilesWithCompletedRefusal(t *testing.T) {
	wire := contractSSE(
		`{"type":"response.refusal.delta","output_index":0,"content_index":0,"delta":"Cannot "}`,
		contractItemEvent(0, `{"type":"message","id":"refusal-1","role":"assistant","content":[{"type":"refusal","refusal":"Cannot help"}]}`),
		contractCompleted("resp-refusal", ""),
	)
	var visible strings.Builder
	result, err := Read(strings.NewReader(wire), func(delta Delta) error { visible.WriteString(delta.Content); return nil }, nil)
	if err != nil || visible.String() != "Cannot help" || result.Text != visible.String() {
		t.Fatalf("refusal=%q, result=%q, err=%v", visible.String(), result.Text, err)
	}
}

func TestReadPreservesOpaqueReasoningAndMessageMetadata(t *testing.T) {
	reasoning := `{"type":"reasoning","id":"rs-1","summary":[{"type":"summary_text","text":"Checking"}],"content":[{"type":"reasoning_text","text":"opaque content"}],"encrypted_content":"signed+/payload==","future_metadata":{"revision":9007199254740993}}`
	message := `{"type":"message","id":"msg-1","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Working"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}}`
	result, err := Read(strings.NewReader(contractSSE(contractItemEvent(0, reasoning), contractItemEvent(1, message), contractCompleted("r", ""))), nil, nil)
	if err != nil || len(result.Output) != 2 || result.Text != "Working" {
		t.Fatalf("result=%+v, err=%v", result, err)
	}
	contractJSONEqual(t, result.Output[0], []byte(reasoning))
	contractJSONEqual(t, result.Output[1], []byte(message))
}

func TestReadToolCallCompleteArgumentsAndOpaqueMetadata(t *testing.T) {
	item := `{"type":"function_call","id":"fc-1","call_id":"call-1","name":"search","arguments":"{\"query\":\"router\"}","encrypted_function_args":["opaque-signed"],"future_field":true}`
	wire := contractSSE(
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc-1","call_id":"call-1","name":"search","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"query\":"}`,
		contractItemEvent(0, item),
		contractCompleted("resp-tool", ""),
	)
	var calls []ToolCall
	result, err := Read(strings.NewReader(wire), func(delta Delta) error { calls = append(calls, delta.ToolCalls...); return nil }, nil)
	if err != nil || len(result.Tools) != 1 || result.Tools[0].ID != "call-1" || result.Tools[0].Function.Name != "search" {
		t.Fatalf("result=%+v, err=%v", result, err)
	}
	var arguments strings.Builder
	for _, call := range calls {
		arguments.WriteString(call.Function.Arguments)
	}
	if arguments.String() != `{"query":"router"}` || result.Tools[0].Function.Arguments != arguments.String() {
		t.Fatalf("tool arguments=%q, final=%q", arguments.String(), result.Tools[0].Function.Arguments)
	}
	contractJSONEqual(t, result.Output[0], []byte(item))
}

func TestReadMalformedToolArgumentsNeverEmitExecutableCall(t *testing.T) {
	for _, arguments := range []string{`{"broken":`, `null`, `[]`, `{"a":1} {"b":2}`} {
		t.Run(arguments, func(t *testing.T) {
			item, err := json.Marshal(map[string]any{"type": "function_call", "id": "fc-1", "call_id": "call-1", "name": "search", "arguments": arguments})
			if err != nil {
				t.Fatal(err)
			}
			emitted := false
			_, err = Read(strings.NewReader(contractSSE(contractItemEvent(0, string(item)), contractCompleted("r", ""))), func(delta Delta) error { emitted = emitted || len(delta.ToolCalls) > 0; return nil }, nil)
			if err == nil || emitted {
				t.Fatalf("malformed tool accepted or emitted: err=%v emitted=%t", err, emitted)
			}
		})
	}
}

func TestReadRequiresValidTerminalResponse(t *testing.T) {
	for _, wire := range []string{
		contractSSE(`{"type":"response.output_text.delta","delta":"partial"}`),
		"data: [DONE]\n\n",
		contractSSE(`{"type":"response.completed"}`),
		contractSSE(`{"type":"response.completed","response":{}}`),
		contractSSE(`{"type":"response.completed","response":{"id":"r","status":"failed"}}`),
		contractSSE(`{"type":"response.incomplete","response":{"id":"r","incomplete_details":{"reason":"max_output_tokens"}}}`),
		"data: malformed\n\n",
	} {
		if _, err := Read(strings.NewReader(wire), nil, nil); err == nil {
			t.Errorf("invalid terminal response accepted: %s", wire)
		}
	}
}

func TestReadInterruptedRequestsContinuation(t *testing.T) {
	result, err := Read(strings.NewReader(contractSSE(`{"type":"response.incomplete","response":{"id":"r","status":"incomplete","incomplete_details":{"reason":"interrupted"}}}`)), nil, nil)
	if err != nil || result.EndTurn == nil || *result.EndTurn {
		t.Fatalf("interrupted end_turn=%v, err=%v", result.EndTurn, err)
	}
}

func TestReadFailsExplicitlyOnUnrepresentableOutput(t *testing.T) {
	for _, item := range []string{
		`{"type":"future_native_tool","id":"new-1","input":"work"}`,
		`{"type":"custom_tool_call","id":"custom-1","call_id":"call-1","name":"unadvertised","input":"opaque"}`,
		`{"type":"message","role":"assistant","content":[{"type":"future_media","payload":"opaque"}]}`,
	} {
		if _, err := Read(strings.NewReader(contractSSE(contractItemEvent(0, item), contractCompleted("r", ""))), nil, nil); err == nil {
			t.Errorf("unsupported output silently lost: %s", item)
		}
	}
}

func TestReadTerminalErrorsAreNotSuccessfulEmptyMessages(t *testing.T) {
	for _, tt := range []struct {
		kind, code string
		status     int
	}{
		{"response.failed", "context_length_exceeded", 400},
		{"response.failed", "invalid_prompt", 400},
		{"response.failed", "rate_limit_exceeded", 429},
		{"response.failed", "bio_policy", 403},
		{"error", "flex_unavailable", 503},
	} {
		t.Run(tt.code, func(t *testing.T) {
			wire := `{"type":"` + tt.kind + `","error":{"code":"` + tt.code + `"},"response":{"id":"r","error":{"code":"` + tt.code + `"}}}`
			_, err := Read(strings.NewReader(contractSSE(wire)), nil, nil)
			var protocol *ProtocolError
			if !errors.As(err, &protocol) || protocol.Status != tt.status {
				t.Fatalf("error=%v, want upstream classification status %d", err, tt.status)
			}
		})
	}
}

func TestReadUnknownTelemetryDoesNotBreakSupportedOutput(t *testing.T) {
	wire := contractSSE(
		`{"type":"codex.future_timing","timing":{"ms":12}}`,
		`{"type":"response.future_optional.delta","delta":"not user-visible"}`,
		contractItemEvent(0, `{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`),
		contractCompleted("r", ""),
	)
	result, err := Read(strings.NewReader(wire), nil, nil)
	if err != nil || result.Text != "answer" {
		t.Fatalf("unknown telemetry broke output: text=%q err=%v", result.Text, err)
	}
}

type contractUnexpectedRead struct{}

func (contractUnexpectedRead) Read([]byte) (int, error) {
	return 0, errors.New("read beyond terminal event")
}

func TestReadStopsAtTerminalEventWithoutWaitingForEOF(t *testing.T) {
	reader := io.MultiReader(strings.NewReader(contractSSE(contractCompleted("r", ""))), contractUnexpectedRead{})
	if _, err := Read(reader, nil, nil); err != nil {
		t.Fatal(err)
	}
}
