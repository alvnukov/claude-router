package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func contractHTTPResponse(body, turnState string) *http.Response {
	headers := make(http.Header)
	if turnState != "" {
		headers.Set("x-codex-turn-state", turnState)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(body))}
}

func TestExchangeContinuesNativeStateAndAggregatesSamplingUsage(t *testing.T) {
	reasoning := `{"type":"reasoning","id":"reasoning-1","summary":[],"encrypted_content":"signed-continuation"}`
	commentary := `{"type":"message","id":"message-1","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"One"}]}`
	final := `{"type":"message","id":"message-2","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Two"}]}`
	replies := []string{
		contractSSE(contractItemEvent(0, reasoning), contractItemEvent(1, commentary), contractCompleted("r1", `,"end_turn":false,"usage":{"input_tokens":1000,"output_tokens":5,"total_tokens":1005,"input_tokens_details":{"cached_tokens":900,"cache_write_tokens":10}}`)),
		contractSSE(contractItemEvent(0, final), contractCompleted("r2", `,"end_turn":true,"usage":{"input_tokens":1020,"output_tokens":7,"total_tokens":1027,"input_tokens_details":{"cached_tokens":1000,"cache_write_tokens":0}}`)),
	}
	var payloads [][]byte
	var requestHeaders []http.Header
	send := func(_ context.Context, payload []byte, headers http.Header) (*http.Response, error) {
		payloads = append(payloads, append([]byte(nil), payload...))
		requestHeaders = append(requestHeaders, headers.Clone())
		if len(payloads) > len(replies) {
			return nil, errors.New("unexpected extra sampling call")
		}
		return contractHTTPResponse(replies[len(payloads)-1], "sticky-token"), nil
	}
	var emitted strings.Builder
	result, err := Exchange(context.Background(), contractRequest(contractUser), send, Options{SessionKey: "stable-session"}, func(delta Delta) error {
		emitted.WriteString(delta.Content)
		return nil
	})
	if err != nil || len(payloads) != 2 || result.Calls != 2 || result.Text != "OneTwo" || emitted.String() != result.Text {
		t.Fatalf("calls=%d result=%+v emitted=%q err=%v", len(payloads), result, emitted.String(), err)
	}
	input := contractInputs(t, payloads[1])
	if len(input) != 3 {
		t.Fatalf("continuation has %d input items, want 3", len(input))
	}
	contractJSONEqual(t, input[0], []byte(contractUser))
	contractJSONEqual(t, input[1], []byte(reasoning))
	contractJSONEqual(t, input[2], []byte(commentary))
	var continued map[string]json.RawMessage
	if err := json.Unmarshal(payloads[1], &continued); err != nil {
		t.Fatal(err)
	}
	contractJSONEqual(t, continued["reasoning"], []byte(`{"effort":"high"}`))
	contractJSONEqual(t, continued["prompt_cache_key"], []byte(`"cache-key"`))
	for i, headers := range requestHeaders {
		if headers.Get("session-id") != "stable-session" || headers.Get("thread-id") != "stable-session" || headers.Get("Accept") != "text/event-stream" {
			t.Errorf("request %d lost session headers: %v", i, headers)
		}
	}
	if requestHeaders[0].Get("x-codex-turn-state") != "" || requestHeaders[1].Get("x-codex-turn-state") != "sticky-token" || result.TurnState != "sticky-token" {
		t.Fatalf("turn-state lifecycle incorrect: requests=%v result=%q", requestHeaders, result.TurnState)
	}
	if !result.UsageKnown || result.Usage.InputTokens != 2020 || result.Usage.OutputTokens != 12 || result.LastUsage.InputTokens != 1020 {
		t.Fatalf("sampling usage was lost or last context inflated: total=%+v last=%+v", result.Usage, result.LastUsage)
	}
	if result.Usage.InputTokenDetails == nil || result.Usage.InputTokenDetails.CachedTokens == nil || *result.Usage.InputTokenDetails.CachedTokens != 1900 {
		t.Fatalf("cache usage not aggregated: %+v", result.Usage.InputTokenDetails)
	}
}

func TestExchangeDoesNotContinueBeforeClientExecutesTool(t *testing.T) {
	calls := 0
	send := func(context.Context, []byte, http.Header) (*http.Response, error) {
		calls++
		return contractHTTPResponse(contractSSE(
			contractItemEvent(0, `{"type":"function_call","id":"fc-1","call_id":"call-1","name":"search","arguments":"{}"}`),
			contractCompleted("r", `,"end_turn":false`),
		), ""), nil
	}
	result, err := Exchange(context.Background(), contractRequest(contractUser), send, Options{}, nil)
	if err != nil || calls != 1 || len(result.Tools) != 1 {
		t.Fatalf("tool sampling advanced without result: calls=%d tools=%d err=%v", calls, len(result.Tools), err)
	}
}

func TestExchangeInterruptedResponseContinuesWithRetainedOutput(t *testing.T) {
	calls := 0
	send := func(_ context.Context, payload []byte, _ http.Header) (*http.Response, error) {
		calls++
		if calls == 1 {
			return contractHTTPResponse(contractSSE(contractItemEvent(0, `{"type":"reasoning","id":"rs","summary":[],"encrypted_content":"state"}`),
				`{"type":"response.incomplete","response":{"id":"r1","incomplete_details":{"reason":"interrupted"}}}`), ""), nil
		}
		if !strings.Contains(string(payload), "encrypted_content") {
			return nil, errors.New("interrupted native state lost")
		}
		return contractHTTPResponse(contractSSE(contractItemEvent(0, `{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"done"}]}`), contractCompleted("r2", "")), ""), nil
	}
	result, err := Exchange(context.Background(), contractRequest(contractUser), send, Options{}, nil)
	if err != nil || calls != 2 || result.Text != "done" {
		t.Fatalf("interrupted continuation: calls=%d text=%q err=%v", calls, result.Text, err)
	}
}

func TestExchangeBoundsInvalidContinuations(t *testing.T) {
	for _, tt := range []struct {
		name, output, id string
		wantCalls        int
	}{
		{"no output progress", "", "r", 1},
		{"repeated response", contractItemEvent(0, `{"type":"reasoning","id":"rs","summary":[],"encrypted_content":"state"}`), "same", 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			send := func(context.Context, []byte, http.Header) (*http.Response, error) {
				calls++
				wire := contractSSE(contractCompleted(tt.id, `,"end_turn":false`))
				if tt.output != "" {
					wire = contractSSE(tt.output, contractCompleted(tt.id, `,"end_turn":false`))
				}
				return contractHTTPResponse(wire, ""), nil
			}
			_, err := Exchange(context.Background(), contractRequest(contractUser), send, Options{MaxContinuations: 2}, nil)
			if err == nil || calls != tt.wantCalls {
				t.Fatalf("bad continuation accepted or looped: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestExchangePreservesOriginalHTTPStatusAfterCodeMapping(t *testing.T) {
	send := func(context.Context, []byte, http.Header) (*http.Response, error) {
		response := contractHTTPResponse(`{"error":{"code":"rate_limit_exceeded"}}`, "")
		response.StatusCode = http.StatusServiceUnavailable
		return response, nil
	}
	_, err := Exchange(context.Background(), contractRequest(contractUser), send, Options{}, nil)
	var protocol *ProtocolError
	if !errors.As(err, &protocol) || protocol.Status != http.StatusTooManyRequests {
		t.Fatalf("mapped status = %v; want 429", err)
	}
	original := reflect.ValueOf(protocol).Elem().FieldByName("HTTPStatus")
	if !original.IsValid() || original.Int() != http.StatusServiceUnavailable {
		t.Fatalf("original HTTP status lost after mapping: %+v", protocol)
	}
}

func TestExchangeSSEFailureDoesNotInventOriginalHTTPStatus(t *testing.T) {
	for _, event := range []string{
		`{"type":"error","error":{"code":"rate_limit_exceeded"}}`,
		`{"type":"response.failed","response":{"id":"r","error":{"code":"insufficient_quota"}}}`,
	} {
		t.Run(event, func(t *testing.T) {
			send := func(context.Context, []byte, http.Header) (*http.Response, error) {
				return contractHTTPResponse(contractSSE(event), ""), nil
			}
			_, err := Exchange(context.Background(), contractRequest(contractUser), send, Options{}, nil)
			var protocol *ProtocolError
			if !errors.As(err, &protocol) || protocol.Status != http.StatusTooManyRequests || protocol.HTTPStatus != 0 {
				t.Fatalf("SSE error gained original HTTP status: %+v", protocol)
			}
		})
	}
}

func TestExchangeRejectsDuplicateAndOversizedPublicRetryHeaders(t *testing.T) {
	for _, tt := range []struct {
		name    string
		headers http.Header
	}{
		{"duplicate canonical", http.Header{"Retry-After": {"7", "8"}}},
		{"duplicate case variants", http.Header{"Retry-After": {"7"}, "retry-after": {"8"}}},
		{"oversized", http.Header{"Retry-After": {strings.Repeat("1", 129)}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			send := func(context.Context, []byte, http.Header) (*http.Response, error) {
				response := contractHTTPResponse(`{"error":{"code":"rate_limit_exceeded"}}`, "")
				response.StatusCode, response.Header = http.StatusTooManyRequests, tt.headers
				return response, nil
			}
			_, err := Exchange(context.Background(), contractRequest(contractUser), send, Options{}, nil)
			if got := ClassifyFailure(err, http.StatusTooManyRequests); got.RetryAfter != 0 {
				t.Fatalf("ambiguous public Retry-After = %s", got.RetryAfter)
			}
		})
	}
}

func TestExchangeHTTPRateLimitPreservesRetryAfterWithoutRetry(t *testing.T) {
	calls := 0
	send := func(context.Context, []byte, http.Header) (*http.Response, error) {
		calls++
		response := contractHTTPResponse(`{"error":{"code":"rate_limit_exceeded","message":"private prompt must not leak"}}`, "")
		response.StatusCode = http.StatusTooManyRequests
		response.Header.Set("Retry-After", "7")
		return response, nil
	}
	_, err := Exchange(context.Background(), contractRequest(contractUser), send, Options{}, nil)
	var protocol *ProtocolError
	if !errors.As(err, &protocol) || protocol.Status != 429 || protocol.RetryAfter != 7*time.Second || calls != 1 || strings.Contains(err.Error(), "private prompt") {
		t.Fatalf("bad HTTP failure handling: calls=%d err=%+v", calls, err)
	}
}

type contractCancelledReader struct{ ctx context.Context }

func (r contractCancelledReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func TestExchangeFirstEventTimeoutIsNotSatisfiedByKeepalive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	send := func(ctx context.Context, _ []byte, _ http.Header) (*http.Response, error) {
		body := io.MultiReader(strings.NewReader(": keepalive\n\n"), contractCancelledReader{ctx})
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(body)}, nil
	}
	_, err := Exchange(ctx, contractRequest(contractUser), send, Options{FirstEventTimeout: 10 * time.Millisecond}, nil)
	var protocol *ProtocolError
	if !errors.As(err, &protocol) || protocol.Code != "first_event_timeout" {
		t.Fatalf("keepalive satisfied progress deadline: %v", err)
	}
}

func TestExchangeHonorsCancellationBeforeSend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sent := false
	_, err := Exchange(ctx, contractRequest(contractUser), func(context.Context, []byte, http.Header) (*http.Response, error) {
		sent = true
		return nil, errors.New("must not send")
	}, Options{}, nil)
	if !errors.Is(err, context.Canceled) || sent {
		t.Fatalf("cancellation ignored: sent=%t err=%v", sent, err)
	}
}
