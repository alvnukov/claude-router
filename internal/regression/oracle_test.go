package regression_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

type observedCall struct {
	Path   string
	Model  string
	Effort string
	Body   []byte
}

type expectedCall struct {
	Path   string
	Model  string
	Effort string
	Body   []byte
}

type semanticEvent struct {
	Name string
	Data any
}

func parseJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing JSON: %v", err)
	}
	return value, nil
}

func compareJSON(observed, expected []byte) error {
	got, err := parseJSON(observed)
	if err != nil {
		return fmt.Errorf("observed JSON: %w", err)
	}
	want, err := parseJSON(expected)
	if err != nil {
		return fmt.Errorf("expected JSON: %w", err)
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("JSON mismatch: observed %s, expected %s", observed, expected)
	}
	return nil
}

func compareCalls(observed []observedCall, expected []expectedCall) error {
	if len(observed) != len(expected) {
		return fmt.Errorf("upstream attempts: observed %d, expected %d", len(observed), len(expected))
	}
	for i := range expected {
		got, want := observed[i], expected[i]
		if got.Path != want.Path || got.Model != want.Model || got.Effort != want.Effort {
			return fmt.Errorf("attempt %d: observed %q/%q/%q, expected %q/%q/%q", i, got.Path, got.Model, got.Effort, want.Path, want.Model, want.Effort)
		}
		if want.Body != nil {
			if err := compareJSON(got.Body, want.Body); err != nil {
				return fmt.Errorf("attempt %d body: %w", i, err)
			}
		}
	}
	return nil
}

func compareAttemptOrder(observed, expected []string) error {
	if len(observed) != len(expected) {
		return fmt.Errorf("upstream attempt order: %d observed, %d expected", len(observed), len(expected))
	}
	for i := range expected {
		if observed[i] != expected[i] {
			return fmt.Errorf("upstream attempt %d: observed %q, expected %q", i, observed[i], expected[i])
		}
	}
	return nil
}

func parseEvents(raw []byte) ([]semanticEvent, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid UTF-8 in event stream")
	}
	reader := bufio.NewReader(bytes.NewReader(raw))
	var events []semanticEvent
	var name string
	var data []string
	flush := func() error {
		if name == "" && len(data) == 0 {
			return nil
		}
		if name == "" || len(data) == 0 {
			return errors.New("incomplete SSE event")
		}
		value, err := parseJSON([]byte(strings.Join(data, "\n")))
		if err != nil {
			return fmt.Errorf("%s data: %w", name, err)
		}
		events = append(events, semanticEvent{Name: name, Data: value})
		name, data = "", nil
		return nil
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		switch {
		case line == "":
			if flushErr := flush(); flushErr != nil {
				return nil, flushErr
			}
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case strings.HasPrefix(line, ":"):
		default:
			return nil, fmt.Errorf("unexpected SSE field %q", line)
		}
		if errors.Is(err, io.EOF) {
			if name != "" || len(data) != 0 {
				return nil, errors.New("unterminated SSE event")
			}
			return events, nil
		}
	}
}

func compareEvents(observed, expected []byte) error {
	got, err := parseEvents(observed)
	if err != nil {
		return fmt.Errorf("observed SSE: %w", err)
	}
	want, err := parseEvents(expected)
	if err != nil {
		return fmt.Errorf("expected SSE: %w", err)
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("SSE events: observed %v, expected %v", got, want)
	}
	return nil
}

func TestRegressionOracleRejectsCorruptedObservations(t *testing.T) {
	original := []byte(`{"type":"message","content":[{"type":"tool_use","id":"tool_fixture_1","name":"fixture_lookup","input":{"n":7}}],"stop_reason":"tool_use"}`)
	for _, observed := range [][]byte{
		[]byte(`{"type":"message","content":[],"stop_reason":"tool_use"}`),
		[]byte(`{"type":"message","content":[{"type":"tool_use","id":"tool_fixture_1","name":"fixture_lookup","input":{"n":8}}],"stop_reason":"tool_use"}`),
		[]byte(`{"type":"message","content":[{"type":"tool_use","id":"tool_fixture_1","name":"fixture_lookup","input":{"n":7}}],"stop_reason":"end_turn"}`),
	} {
		if err := compareJSON(observed, original); err == nil {
			t.Fatal("corrupted observed message passed fixed oracle")
		}
	}
	if err := compareJSON(original, original); err != nil {
		t.Fatal(err)
	}
	calls := []observedCall{{Path: "/chat/completions", Model: "fixture-b-model", Effort: "high"}}
	want := []expectedCall{{Path: "/chat/completions", Model: "fixture-b-model", Effort: "high"}}
	if err := compareCalls(calls, want); err != nil {
		t.Fatal(err)
	}
	for _, broken := range [][]observedCall{
		{{Path: "/chat/completions", Model: "fixture-a-model", Effort: "high"}},
		{{Path: "/chat/completions", Model: "fixture-b-model", Effort: "high"}, {Path: "/chat/completions", Model: "fixture-a-model"}},
	} {
		if err := compareCalls(broken, want); err == nil {
			t.Fatal("corrupted observed attempt journal passed fixed oracle")
		}
	}
}

func TestRegressionOracleSSERejectsMissingTerminalAndWrongTool(t *testing.T) {
	expected := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool_fixture_1\",\"name\":\"fixture_lookup\",\"input\":{}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"n\\\":7}\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	if err := compareEvents(expected, expected); err != nil {
		t.Fatal(err)
	}
	for _, observed := range [][]byte{
		bytes.Replace(expected, []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"), nil, 1),
		bytes.Replace(expected, []byte(`\"n\":7`), []byte(`\"n\":8`), 1),
		bytes.Replace(expected, []byte("event: content_block_delta"), []byte("event: content_block_stop"), 1),
	} {
		if err := compareEvents(observed, expected); err == nil {
			t.Fatal("corrupted observed stream passed fixed semantic oracle")
		}
	}
}
