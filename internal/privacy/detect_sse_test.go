package privacy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func reasoningSSEFrame(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

func reasoningSSE() string {
	return reasoningSSEFrame("message_start", `{"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","model":"synthetic","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`) +
		reasoningSSEFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`) +
		reasoningSSEFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"private reasoning"}}`) +
		reasoningSSEFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque-signature"}}`) +
		reasoningSSEFrame("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		reasoningSSEFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`) +
		reasoningSSEFrame("message_stop", `{"type":"message_stop"}`)
}

func TestUnmaskedTrafficSSEReasoning(t *testing.T) {
	thinking := reasoningSSE()
	redacted := strings.Replace(thinking, `{"type":"thinking","thinking":"","signature":""}`, `{"type":"redacted_thinking","data":"opaque-data"}`, 1)
	start := strings.Index(redacted, "event: content_block_delta")
	stop := strings.Index(redacted, "event: content_block_stop")
	redacted = redacted[:start] + redacted[stop:]
	for name, stream := range map[string]string{"thinking": thinking, "redacted": redacted, "text-and-tool": compatSSE("plain")} {
		t.Run(name, func(t *testing.T) {
			if err := checkUnmaskedTrafficSSE([]byte(stream)); err != nil {
				t.Fatalf("unmasked valid stream rejected: %v", err)
			}
		})
	}
	for _, stream := range []string{thinking, redacted} {
		if err := (*Exchange)(nil).checkTrafficSSE([]byte(stream)); err == nil {
			t.Fatal("strict SSE accepted opaque reasoning")
		}
	}
}

func TestUnmaskedTrafficSSERejectsMalformedReasoning(t *testing.T) {
	good := reasoningSSE()
	for name, stream := range map[string]string{
		"missing-terminal":      strings.TrimSuffix(good, reasoningSSEFrame("message_stop", `{"type":"message_stop"}`)),
		"open-block":            strings.Replace(good, reasoningSSEFrame("content_block_stop", `{"type":"content_block_stop","index":0}`), "", 1),
		"before-message":        good[strings.Index(good, "event: content_block_start"):],
		"duplicate-block":       strings.Replace(good, "event: message_delta", reasoningSSEFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)+"event: message_delta", 1),
		"error":                 strings.Replace(good, "event: message_stop", reasoningSSEFrame("error", `{"type":"error","error":{"type":"overloaded_error"}}`)+"event: message_stop", 1),
		"after-terminal":        good + reasoningSSEFrame("ping", `{"type":"ping"}`),
		"wrong-block-field":     strings.Replace(good, `"thinking":"","signature":""`, `"thinking":4,"signature":""`, 1),
		"wrong-delta-field":     strings.Replace(good, `"thinking":"private reasoning"`, `"thinking":4`, 1),
		"wrong-signature-field": strings.Replace(good, `"signature":"opaque-signature"`, `"signature":null`, 1),
		"duplicate-json-key":    strings.Replace(good, `"signature":"opaque-signature"`, `"signature":"opaque-signature","signature":"another"`, 1),
		"invalid-utf8":          strings.Replace(good, "private reasoning", string([]byte{255}), 1),
		"invalid-index":         strings.ReplaceAll(good, `"index":0`, `"index":"zero"`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkUnmaskedTrafficSSE([]byte(stream)); err == nil {
				t.Fatal("malformed stream admitted")
			}
		})
	}
}

func TestReasoningLifecycleTracksOpenBlock(t *testing.T) {
	life := &protectedLifecycle{clock: newCompatClock(), limits: LifecycleLimits{Idle: time.Second}}
	open := map[string]string{}
	start := reasoningSSEFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`)
	observeProtectedFrame([]byte(strings.TrimSuffix(start, "\n\n")), open, life)
	terminal := []byte(`event: message_stop` + "\n" + `data: {"type":"message_stop"}`)
	if observeProtectedFrame(terminal, open, life) {
		t.Fatal("message_stop completed with an open thinking block")
	}
	observeProtectedFrame([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}"), open, life)
	if !observeProtectedFrame(terminal, open, life) {
		t.Fatal("closed thinking block prevented terminal")
	}
}

func TestReasoningLifecycleDeltaExtendsIdle(t *testing.T) {
	for _, delta := range []string{`{"type":"thinking_delta","thinking":"new reasoning"}`, `{"type":"signature_delta","signature":"new signature"}`} {
		t.Run(delta, func(t *testing.T) {
			clock := newCompatClock()
			life := &protectedLifecycle{clock: clock, limits: LifecycleLimits{Idle: 5 * time.Second, Headers: time.Minute, Total: time.Minute}, controller: http.NewResponseController(httptest.NewRecorder())}
			life.begin(context.Background(), func() {})
			defer life.stop()
			life.gotHeaders(nil)
			clock.advance(4 * time.Second)
			frame := reasoningSSEFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":`+delta+`}`)
			observeProtectedFrame([]byte(strings.TrimSuffix(frame, "\n\n")), map[string]string{"0": "thinking"}, life)
			clock.advance(4 * time.Second)
			if life.expired.Load() {
				t.Fatal("useful reasoning delta did not reset idle deadline")
			}
			clock.advance(2 * time.Second)
			if !life.expired.Load() {
				t.Fatal("reasoning reset disabled idle deadline")
			}
		})
	}
}

func TestReasoningLifecycleIgnoresEmptyAndInvalidDelta(t *testing.T) {
	for _, delta := range []string{
		`{"type":"thinking_delta","thinking":""}`,
		`{"type":"signature_delta","signature":""}`,
		`{"type":"thinking_delta","thinking":4}`,
		`{"type":"text_delta","text":"wrong block"}`,
	} {
		t.Run(delta, func(t *testing.T) {
			clock := newCompatClock()
			life := &protectedLifecycle{clock: clock, limits: LifecycleLimits{Idle: 5 * time.Second, Headers: time.Minute, Total: time.Minute}, controller: http.NewResponseController(httptest.NewRecorder())}
			life.begin(context.Background(), func() {})
			defer life.stop()
			life.gotHeaders(nil)
			clock.advance(4 * time.Second)
			frame := reasoningSSEFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":`+delta+`}`)
			observeProtectedFrame([]byte(strings.TrimSuffix(frame, "\n\n")), map[string]string{"0": "thinking"}, life)
			clock.advance(2 * time.Second)
			if !life.expired.Load() {
				t.Fatal("empty or invalid reasoning delta extended idle deadline")
			}
		})
	}
}
