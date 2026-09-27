package regression_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const wireModel = "claude-sonnet-4-5-20250929"

func wireFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "wire", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func observedMessage(t *testing.T, body []byte) map[string]any {
	t.Helper()
	value, err := parseJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	message, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected JSON message object, got %T", value)
	}
	return message
}

func assertMessageFields(t *testing.T, body []byte, blocks []any, stop string) {
	t.Helper()
	message := observedMessage(t, body)
	for key, expected := range map[string]any{"type": "message", "role": "assistant", "model": wireModel, "stop_reason": stop} {
		if !reflect.DeepEqual(message[key], expected) {
			t.Errorf("%s = %v, expected %v", key, message[key], expected)
		}
	}
	if !reflect.DeepEqual(message["content"], blocks) {
		t.Errorf("RR-REQ-01/02/03: blocks differ: got %v, expected %v", message["content"], blocks)
	}
	if !reflect.DeepEqual(message["usage"], map[string]any{"input_tokens": json.Number("4"), "output_tokens": json.Number("2")}) {
		t.Errorf("RR-REQ-01/02/03: usage differs: %v", message["usage"])
	}
	id, ok := message["id"].(string)
	if !ok || !strings.HasPrefix(id, "msg_") {
		t.Errorf("RR-REQ-01/02/03: missing generated message ID")
	}
}

func TestRegressionMessageShapes(t *testing.T) {
	cases := []struct {
		name, fixture, text, stop string
		blocks                    []any
	}{
		{"RR-REQ-01/text", "openai-text.json", "OK-T", "end_turn", []any{map[string]any{"type": "text", "text": "OK-T"}}},
		{"RR-REQ-02/tool_only", "openai-tool.json", "", "tool_use", []any{map[string]any{"type": "tool_use", "id": "tool_fixture_1", "name": "fixture_lookup", "input": map[string]any{"n": json.Number("7")}}}},
		{"RR-REQ-03/literal_JSON", "openai-text.json", "{\"x\":1}", "end_turn", []any{map[string]any{"type": "text", "text": "{\"x\":1}"}}},
	}
	for _, row := range cases {
		t.Run(row.name, func(t *testing.T) {
			stand := startRouter(t, testFixture{})
			if row.name == "RR-REQ-03/literal_JSON" {
				stand.a.setReply(http.StatusOK, "application/json", []byte(`{"choices":[{"message":{"content":"{\"x\":1}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`))
			} else {
				stand.a.setReply(http.StatusOK, "application/json", wireFixture(t, row.fixture))
			}
			status, body := stand.clientCall(t, "/v1/messages", requestWithEffort(wireModel, "default", "wire-fixture", false))
			if status != http.StatusOK {
				t.Fatalf("%s: status %d (wanted 200)", row.name, status)
			}
			assertMessageFields(t, body, row.blocks, row.stop)
			want := []expectedCall{{Path: "/v1/chat/completions", Model: "fixture-a-model", Body: []byte(`{"model":"fixture-a-model","messages":[{"role":"user","content":"READY-T"}],"max_tokens":64}`)}}
			if err := compareCalls(stand.a.allCalls(), want); err != nil {
				t.Errorf("%s: upstream wire: %v", row.name, err)
			}
			if err := compareCalls(stand.b.allCalls(), nil); err != nil {
				t.Errorf("%s: forbidden B call: %v", row.name, err)
			}
		})
	}
}

func TestRegressionLocalOpenAIWire(t *testing.T) {
	stand := startRouter(t, testFixture{})
	request := []byte(`{"model":"claude-sonnet-4-5-20250929","system":"System-T","messages":[{"role":"user","content":"Find-T"}],"max_tokens":64,"tools":[{"name":"fixture_lookup","description":"Synthetic only","input_schema":{"type":"object","properties":{"n":{"type":"integer"}}}}],"tool_choice":{"type":"tool","name":"fixture_lookup"}}`)
	stand.a.setReply(http.StatusOK, "application/json", wireFixture(t, "openai-tool.json"))
	status, body := stand.clientCall(t, "/v1/messages", request)
	if status != http.StatusOK {
		t.Fatalf("RR-LOC-01: status %d, want 200", status)
	}
	assertMessageFields(t, body, []any{map[string]any{"type": "tool_use", "id": "tool_fixture_1", "name": "fixture_lookup", "input": map[string]any{"n": json.Number("7")}}}, "tool_use")
	want := []expectedCall{{Path: "/v1/chat/completions", Model: "fixture-a-model", Body: []byte(`{"model":"fixture-a-model","messages":[{"role":"system","content":"System-T"},{"role":"user","content":"Find-T"}],"max_tokens":64,"tools":[{"type":"function","function":{"name":"fixture_lookup","description":"Synthetic only","parameters":{"type":"object","properties":{"n":{"type":"integer"}}}}}],"tool_choice":{"type":"function","function":{"name":"fixture_lookup"}}}`)}}
	if err := compareCalls(stand.a.allCalls(), want); err != nil {
		t.Errorf("RR-LOC-01: OpenAI request: %v", err)
	}
	if len(stand.b.allCalls()) != 0 || len(stand.cloud.allCalls()) != 0 {
		t.Error("RR-LOC-01: unrelated upstream called")
	}
}

func normalizedEvents(raw []byte) ([]semanticEvent, error) {
	events, err := parseEvents(raw)
	if err != nil || len(events) == 0 {
		return events, err
	}
	first, ok := events[0].Data.(map[string]any)
	if !ok || events[0].Name != "message_start" {
		return nil, errors.New("first event is not message_start")
	}
	message, ok := first["message"].(map[string]any)
	if !ok {
		return nil, errors.New("message_start lacks message")
	}
	id, ok := message["id"].(string)
	if !ok || !strings.HasPrefix(id, "msg_") {
		return nil, errors.New("message_start lacks generated ID")
	}
	message["id"] = "<dynamic>"
	return events, nil
}

func checkStreamAgainstFixture(observed, fixed []byte) error {
	got, err := normalizedEvents(observed)
	if err != nil {
		return fmt.Errorf("observed semantic SSE: %w", err)
	}
	want, err := parseEvents(fixed)
	if err != nil {
		return fmt.Errorf("fixed SSE fixture: %w", err)
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("semantic SSE events differ: observed %v, expected %v", got, want)
	}
	return nil
}

func TestRegressionSSEEventsAndFinal(t *testing.T) {
	for _, row := range []struct{ name, upstream, expected string }{
		{"RR-SSE-01/text", "openai-text.sse", "anthropic-text.sse"},
		{"RR-SSE-02/tool", "openai-tool.sse", "anthropic-tool.sse"},
	} {
		t.Run(row.name, func(t *testing.T) {
			stand := startRouter(t, testFixture{})
			stand.a.setReply(http.StatusOK, "text/event-stream", wireFixture(t, row.upstream))
			status, body := stand.clientCall(t, "/v1/messages", requestWithEffort(wireModel, "default", "sse-fixture", true))
			if status != http.StatusOK {
				t.Fatalf("%s: status %d, want 200", row.name, status)
			}
			fixed := wireFixture(t, row.expected)
			if err := checkStreamAgainstFixture(body, fixed); err != nil {
				t.Errorf("%s: %v", row.name, err)
			}
			wantCall := []expectedCall{{Path: "/v1/chat/completions", Model: "fixture-a-model", Body: []byte(`{"model":"fixture-a-model","messages":[{"role":"user","content":"READY-T"}],"max_tokens":64,"stream":true,"stream_options":{"include_usage":true}}`)}}
			if err := compareCalls(stand.a.allCalls(), wantCall); err != nil {
				t.Errorf("%s: OpenAI stream request: %v", row.name, err)
			}
			if len(stand.b.allCalls()) != 0 {
				t.Error("unexpected extra upstream attempt")
			}
		})
	}
}

func TestRegressionSSEOracleRejectsCorruptedObservation(t *testing.T) {
	fixed := wireFixture(t, "anthropic-tool.sse")
	original := bytes.Replace(fixed, []byte(`"<dynamic>"`), []byte(`"msg_fixture"`), 1)
	if err := checkStreamAgainstFixture(original, fixed); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{
		bytes.Replace(original, []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"), nil, 1),
		bytes.Replace(original, []byte(`"partial_json":"7}"`), []byte(`"partial_json":"8}"`), 1),
		bytes.Replace(original, []byte(`"name":"fixture_lookup"`), []byte(`"name":"other"`), 1),
	} {
		if err := checkStreamAgainstFixture(bad, fixed); err == nil {
			t.Error("corrupted observed SSE passed immutable semantic oracle")
		}
	}
}

func TestRegressionStreamFailurePhases(t *testing.T) {
	t.Run("RR-SSE-03/before_first_usable_chunk", func(t *testing.T) {
		stand := startRouter(t, testFixture{})
		stand.a.setReply(http.StatusServiceUnavailable, "application/json", []byte(`{"error":"fixture unavailable"}`))
		status, body := stand.clientCall(t, "/v1/messages", requestWithEffort(wireModel, "default", "before-fixture", true))
		if status != http.StatusServiceUnavailable || observedMessage(t, body)["type"] != "error" {
			t.Errorf("before-delivery upstream rejection: status=%d, expected 503 error", status)
		}
		if len(stand.a.allCalls()) != 1 || len(stand.b.allCalls()) != 0 {
			t.Error("before-delivery attempt journal is not exactly A")
		}
	})
	t.Run("RR-SSE-03/after_delivery_incomplete", func(t *testing.T) {
		stand := startRouter(t, testFixture{})
		stand.a.setReply(http.StatusOK, "text/event-stream", []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK-\"},\"finish_reason\":null}]}\n\n"))
		status, body := stand.clientCall(t, "/v1/messages", requestWithEffort(wireModel, "default", "after-fixture", true))
		if status != http.StatusOK {
			t.Fatalf("status before useful event: %d, want committed 200", status)
		}
		events, err := parseEvents(body)
		if err != nil {
			t.Fatal(err)
		}
		var useful bool
		for _, event := range events {
			if event.Name == "content_block_delta" {
				useful = true
			}
			if event.Name == "message_stop" {
				t.Error("incomplete upstream stream was marked successful")
			}
		}
		if !useful {
			t.Error("no useful event delivered; cannot classify post-delivery failure")
		}
		if len(stand.a.allCalls()) != 1 || len(stand.b.allCalls()) != 0 {
			t.Error("post-delivery failure caused unexpected retry")
		}
	})
}

func TestRegressionCancelNoRetry(t *testing.T) {
	stand := startRouter(t, testFixture{})
	stand.a.setResponder(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OPEN-T\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		select {
		case stand.a.cancelled <- struct{}{}:
		default:
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, stand.apiURL+"/v1/messages", bytes.NewReader(requestWithEffort(wireModel, "default", "cancel-fixture", true)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(request)
	if err != nil {
		t.Fatalf("blocked: client did not reach streaming phase: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream did not begin: status %d", response.StatusCode)
	}
	reader := bufio.NewReader(response.Body)
	var deliveredDelta bool
	for !deliveredDelta {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("blocked: stream did not deliver a useful event: %v", err)
		}
		if line == "event: content_block_delta\n" {
			deliveredDelta = true
		}
	}
	cancel()
	select {
	case <-stand.a.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("RR-SSE-03: upstream context was not canceled after delivered event")
	}
	if len(stand.a.allCalls()) != 1 || len(stand.b.allCalls()) != 0 {
		t.Error("cancellation attempted a different upstream")
	}
}
