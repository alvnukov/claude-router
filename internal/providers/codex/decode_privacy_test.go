package codex

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestReadRejectsConflictingRepeatedItems(t *testing.T) {
	for name, items := range map[string][2]string{
		"tool arguments": {
			`{"type":"function_call","id":"fc","call_id":"call","name":"run","arguments":"{\"command\":\"echo safe\"}"}`,
			`{"type":"function_call","id":"fc","call_id":"call","name":"run","arguments":"{\"command\":\"<secret:credential:deadbeef>\"}"}`,
		},
		"message text": {
			`{"type":"message","id":"msg","content":[{"type":"output_text","text":"first"}]}`,
			`{"type":"message","id":"msg","content":[{"type":"output_text","text":"changed"}]}`,
		},
		"opaque integer metadata": {
			`{"type":"reasoning","id":"rs","revision":9007199254740992}`,
			`{"type":"reasoning","id":"rs","revision":9007199254740993}`,
		},
	} {
		for _, finalOutput := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/item done", true: "/completion output"}[finalOutput], func(t *testing.T) {
				events := []string{contractItemEvent(0, items[0]), contractItemEvent(0, items[1]), contractCompleted("r", "")}
				if finalOutput {
					events = []string{contractItemEvent(0, items[0]), contractCompleted("r", `,"output":[`+items[1]+`]`)}
				}
				_, err := Read(strings.NewReader(contractSSE(events...)), nil, nil)
				var protocol *ProtocolError
				if !errors.As(err, &protocol) || protocol.Code != "output_item_changed" || protocol.Retryable {
					t.Fatalf("conflicting repeated item accepted or wrong error: %v", err)
				}
			})
		}
	}
}

func TestReadDeduplicatesEquivalentRepeatedItems(t *testing.T) {
	first := `{"type":"function_call","id":"fc","call_id":"call","name":"run","arguments":"{\"a\":1,\"b\":2}","metadata":{"revision":9007199254740993,"ok":true}}`
	repeated := `{"metadata": {"ok":true, "revision":9007199254740993}, "arguments":"{ \"b\": 2, \"a\": 1 }", "name":"run","call_id":"call","id":"fc","type":"function_call"}`
	result, err := Read(strings.NewReader(contractSSE(contractItemEvent(0, first), contractItemEvent(0, repeated), contractCompleted("r", `,"output":[`+repeated+`]`))), nil, nil)
	if err != nil || len(result.Tools) != 1 || len(result.Output) != 1 {
		t.Fatalf("equivalent items were rejected or duplicated: tools=%d output=%d err=%v", len(result.Tools), len(result.Output), err)
	}
	contractJSONEqual(t, result.Output[0], []byte(first))
}

func TestReadRejectsMalformedKnownUsage(t *testing.T) {
	for name, usage := range map[string]string{
		"input overflow":       `{"input_tokens":9223372036854775808,"output_tokens":1}`,
		"output type":          `{"input_tokens":1,"output_tokens":"private-canary"}`,
		"partial cached count": `{"input_tokens_details":{"cached_tokens":9223372036854775808}}`,
		"partial input detail": `{"input_tokens_details":[]}`,
		"partial reasoning":    `{"output_tokens_details":{"reasoning_tokens":"private-canary"}}`,
		"partial total":        `{"total_tokens":9223372036854775808}`,
		"partial budget":       `{"codex_rollout_budget_units":true}`,
		"usage shape":          `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Read(strings.NewReader(contractSSE(contractCompleted("r", `,"usage":`+usage))), nil, nil)
			var protocol *ProtocolError
			if !errors.As(err, &protocol) || protocol.Code != "invalid_usage" || protocol.Retryable || strings.Contains(err.Error(), "private-canary") {
				t.Fatalf("malformed usage accepted or unsafe error: %v", err)
			}
		})
	}
}

func TestReadKeepsValidPartialUsageUnknown(t *testing.T) {
	usage := `{"input_tokens":12,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":2},"codex_rollout_budget_units":1.5}`
	result, err := Read(strings.NewReader(contractSSE(contractCompleted("r", `,"usage":`+usage))), nil, nil)
	if err != nil || result.UsageKnown || result.LastUsage.InputTokens != 0 || result.Usage.InputTokens != 0 {
		t.Fatalf("valid partial usage changed semantics: %+v, %v", result, err)
	}
}

func TestStartDoesNotCompleteInvalidFinalProjection(t *testing.T) {
	item := `{"type":"function_call","id":"fc","call_id":"call","name":"run","arguments":"{\"command\":\"echo safe\"}"}`
	changed := strings.Replace(item, "echo safe", "<secret:credential:deadbeef>", 1)
	for name, final := range map[string]string{
		"changed tool":  contractCompleted("r", `,"output":[`+changed+`]`),
		"invalid usage": contractCompleted("r", `,"usage":{"input_tokens":9223372036854775808,"output_tokens":1}`),
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/buffered", true: "/streaming"}[streaming], func(t *testing.T) {
				send := func(context.Context, []byte, http.Header) (*http.Response, error) {
					return contractHTTPResponse(contractSSE(contractItemEvent(0, item), final), ""), nil
				}
				stream := Start(context.Background(), contractRequest(contractUser), send, Options{}, streaming)
				defer stream.Close()
				body, readErr := io.ReadAll(stream)
				_, resultErr := stream.Result()
				if readErr == nil || resultErr == nil || strings.Contains(string(body), "[DONE]") || strings.Contains(string(body), "finish_reason") || !streaming && len(body) != 0 {
					t.Fatalf("invalid final projection appeared complete: body=%s read=%v result=%v", body, readErr, resultErr)
				}
			})
		}
	}
}
