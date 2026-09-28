package privacy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func supportedResponseExchange(t *testing.T) (*Exchange, string) {
	t.Helper()
	rules, err := ParseRules([]byte(`{"entries":[{"kind":"org","forms":["ResponseCanary"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, rules)
	masked, req, err := e.Mask([]byte(`{"system":"ResponseCanary"}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(req.Close)
	e.opt.supportedOnly = true
	var body struct {
		System string `json:"system"`
	}
	if err := json.Unmarshal(masked, &body); err != nil || body.System == "ResponseCanary" {
		t.Fatalf("missing alias: %s (%v)", masked, err)
	}
	return &Exchange{engine: e, request: req, runtime: &Runtime{}}, body.System
}

func TestSupportedResponseMixedJSON(t *testing.T) {
	x, alias := supportedResponseExchange(t)
	a, _ := json.Marshal(alias)
	opaque := `{"type":"future_block", "text":` + string(a) + `,"nested":{"text":` + string(a) + `}}`
	service := `"service": {"text":` + string(a) + `}`
	body := []byte(`{"content":[{"type":"text","text":` + string(a) + `,"future":true},` + opaque + `,{"type":"thinking","thinking":` + string(a) + `,"signature":` + string(a) + `}],` + service + `}`)
	out, err := x.Restore(body, false)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Replace(body, []byte(`"text":`+string(a)), []byte(`"text":"ResponseCanary"`), 1)
	if !bytes.Equal(out, want) {
		t.Fatalf("changed opaque JSON or failed restoration:\n%s\nwant %s", out, want)
	}
	x.engine.opt.supportedOnly = false
	if _, err := x.Restore(body, false); err == nil {
		t.Fatal("strict response accepted unsupported blocks")
	}
}

func TestSupportedResponseMixedSSE(t *testing.T) {
	x, alias := supportedResponseExchange(t)
	a, _ := json.Marshal(alias)
	frame := reasoningSSEFrame
	start := frame("message_start", `{"type":"message_start","message":{"content":[],"future":true},"service":true}`)
	text := frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""},"future":true}`) +
		frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+string(a)+`},"service":true}`) +
		frame("content_block_stop", `{"type":"content_block_stop","index":0}`)
	opaque := frame("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"future_block","text":`+string(a)+`}}`) +
		frame("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":`+string(a)+`}}`) +
		frame("content_block_stop", `{"type":"content_block_stop","index":1}`) +
		frame("future_event", `{"type":"future_event","text":`+string(a)+`}`) +
		frame("service_event", `{"text":`+string(a)+`}`)
	end := frame("message_stop", `{"type":"message_stop","future":true}`)
	body := []byte(start + text + opaque + end)
	out, err := x.Restore(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"text":"ResponseCanary"`)) || !bytes.Contains(out, []byte(opaque)) || !bytes.HasPrefix(out, []byte(start)) || !bytes.HasSuffix(out, []byte(end)) {
		t.Fatalf("known text not restored or opaque frame changed: %s", out)
	}
	if err := checkUnmaskedTrafficSSE(body); err != nil {
		t.Fatalf("unmasked unknown protocol rejected: %v", err)
	}
	x.engine.opt.supportedOnly = false
	if _, err := x.Restore(body, true); err == nil {
		t.Fatal("strict SSE accepted unsupported protocol")
	}
}

func TestSupportedResponseToolInputAndOpaqueFields(t *testing.T) {
	x, alias := supportedResponseExchange(t)
	a, _ := json.Marshal(alias)
	body := []byte(reasoningSSEFrame("message_start", `{"type":"message_start","message":{"content":[]}}`) +
		reasoningSSEFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"opaque-id","name":`+string(a)+`,"input":{"text":`+string(a)+`,`+string(a)+`:42},"future":`+string(a)+`}}`) +
		reasoningSSEFrame("content_block_stop", `{"type":"content_block_stop","index":0}`) + reasoningSSEFrame("message_stop", `{"type":"message_stop"}`))
	out, err := x.Restore(body, true)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Replace(body, []byte(`"input":{"text":`+string(a)), []byte(`"input":{"text":"ResponseCanary"`), 1)
	if !bytes.Equal(out, want) {
		t.Fatalf("initial tool input/opaque fields changed: %s", out)
	}
	if err := x.CheckControl("ResponseCanary"); err != nil {
		t.Fatal("selective mode rejected a structural identifier")
	}
	x.request.Close()
	if err := x.CheckControl("ResponseCanary"); err == nil {
		t.Fatal("closed request accepted")
	}
}

func TestSupportedResponseSSEProtocolErrors(t *testing.T) {
	good := reasoningSSEFrame("message_start", `{"type":"message_start","message":{"content":[]}}`) +
		reasoningSSEFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"future_block"}}`) +
		reasoningSSEFrame("content_block_stop", `{"type":"content_block_stop","index":0}`) + reasoningSSEFrame("message_stop", `{"type":"message_stop"}`)
	for name, body := range map[string]string{
		"truncated":         strings.TrimSuffix(good, reasoningSSEFrame("message_stop", `{"type":"message_stop"}`)),
		"open opaque block": strings.Replace(good, reasoningSSEFrame("content_block_stop", `{"type":"content_block_stop","index":0}`), "", 1),
		"error":             strings.Replace(good, "event: message_stop", reasoningSSEFrame("error", `{"type":"error","error":{}}`)+"event: message_stop", 1),
		"invalid json":      strings.Replace(good, `"type":"future_block"`, `"type":"future_block","x":`, 1),
		"duplicate json":    strings.Replace(good, `"type":"future_block"`, `"type":"future_block","type":"second"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkUnmaskedTrafficSSE([]byte(body)); err == nil {
				t.Fatal("malformed stream accepted")
			}
		})
	}
}

func TestSupportedResponseOpaqueLifecycle(t *testing.T) {
	clock := newCompatClock()
	life := &protectedLifecycle{clock: clock, limits: LifecycleLimits{Idle: 5 * time.Second, Headers: time.Minute, Total: 12 * time.Second}, controller: http.NewResponseController(httptest.NewRecorder())}
	life.begin(context.Background(), func() {})
	defer life.stop()
	life.gotHeaders(nil)
	open := map[string]string{}
	observe := func(event, data string) bool {
		return observeProtectedFrame([]byte(strings.TrimSuffix(reasoningSSEFrame(event, data), "\n\n")), open, life)
	}
	observe("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"future_block"},"service":true}`)
	if observe("message_stop", `{"type":"message_stop","service":true}`) {
		t.Fatal("terminal ignored an open opaque block")
	}
	for range 2 {
		clock.advance(4 * time.Second)
		observe("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"future_delta","data":"progress"},"service":true}`)
		if life.expired.Load() {
			t.Fatal("opaque progress did not reset idle")
		}
	}
	clock.advance(4 * time.Second)
	if !life.expired.Load() {
		t.Fatal("opaque progress extended total deadline")
	}
	closed := &protectedLifecycle{clock: newCompatClock()}
	open = map[string]string{}
	observeProtectedFrame([]byte(`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"future_block"}}`), open, closed)
	observeProtectedFrame([]byte(`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":0,"service":true}`), open, closed)
	if !observeProtectedFrame([]byte(`event: message_stop`+"\n"+`data: {"type":"message_stop","service":true}`), open, closed) {
		t.Fatal("closed opaque block prevented terminal")
	}
}

func TestSupportedResponseRejectsKnownMismatchedDeltas(t *testing.T) {
	for _, block := range []string{"text", "tool_use", "thinking", "future_block"} {
		for _, delta := range []struct{ typ, field, validBlock string }{
			{"text_delta", "text", "text"},
			{"input_json_delta", "partial_json", "tool_use"},
			{"thinking_delta", "thinking", "thinking"},
			{"signature_delta", "signature", "thinking"},
			{"future_delta", "data", ""},
		} {
			t.Run(block+"/"+delta.typ, func(t *testing.T) {
				content := `{"type":"` + block + `"}`
				if block == "thinking" {
					content = `{"type":"thinking","thinking":""}`
				}
				body := reasoningSSEFrame("message_start", `{"type":"message_start","message":{"content":[]}}`) +
					reasoningSSEFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":`+content+`}`) +
					reasoningSSEFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"`+delta.typ+`","`+delta.field+`":"value"}}`) +
					reasoningSSEFrame("content_block_stop", `{"type":"content_block_stop","index":0}`) +
					reasoningSSEFrame("message_stop", `{"type":"message_stop"}`)
				wantError := block != "future_block" && delta.validBlock != "" && block != delta.validBlock
				if err := checkUnmaskedTrafficSSE([]byte(body)); (err != nil) != wantError {
					t.Fatalf("err=%v; want rejection=%t", err, wantError)
				}
			})
		}
	}
}
