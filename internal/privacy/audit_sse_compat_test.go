package privacy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// WHATWG SSE parsing permits comments, ignored fields, multiple data lines,
// CR/LF/CRLF line endings, a leading BOM, and last-wins event fields.
// https://html.spec.whatwg.org/multipage/server-sent-events.html#parsing-an-event-stream
func TestAuditSSEValidFraming(t *testing.T) {
	for _, variant := range []string{"comments", "id-retry", "multiline", "crlf", "cr", "mixed-newlines", "bom", "event-last-wins", "data-only"} {
		t.Run(variant, func(t *testing.T) {
			x, alias := supportedResponseExchange(t)
			a, _ := json.Marshal(alias)
			frame := reasoningSSEFrame
			body := frame("message_start", `{"type":"message_start","message":{"content":[]}}`) +
				frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
				frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+string(a)+`}}`) +
				frame("content_block_stop", `{"type":"content_block_stop","index":0}`) +
				frame("message_stop", `{"type":"message_stop"}`)
			switch variant {
			case "comments":
				body = ": keepalive\n\n" + strings.ReplaceAll(body, "event:", ": comment\nevent:")
			case "id-retry":
				body = "retry: 1000\n\n" + strings.ReplaceAll(body, "event:", "id: synthetic\nretry: 1000\nx-provider: retained\nignored-field\nevent:")
			case "multiline":
				body = strings.ReplaceAll(body, `,"`, ",\ndata: \"")
			case "crlf":
				body = strings.ReplaceAll(body, "\n", "\r\n")
			case "cr":
				body = strings.ReplaceAll(body, "\n", "\r")
			case "mixed-newlines":
				body = strings.ReplaceAll(body, "\n\n", "\r\n\n")
			case "bom":
				body = "\xef\xbb\xbf" + body
			case "event-last-wins":
				body = strings.ReplaceAll(body, "event:", "event: ignored\nevent:")
			case "data-only":
				var out strings.Builder
				for _, line := range strings.SplitAfter(body, "\n") {
					if !strings.HasPrefix(line, "event:") {
						out.WriteString(line)
					}
				}
				body = out.String()
			}
			if err := checkUnmaskedTrafficSSE([]byte(body)); err != nil {
				t.Fatalf("valid framing rejected: %v", err)
			}
			life := &protectedLifecycle{clock: newCompatClock(), limits: LifecycleLimits{Idle: time.Minute}}
			read, err := readProtectedBody(strings.NewReader(body), true, life)
			if err != nil || string(read) != body {
				t.Fatalf("lifecycle altered/rejected stream: %v", err)
			}
			out, err := x.Restore([]byte(body), true)
			if err != nil || !bytes.Contains(out, []byte("ResponseCanary")) {
				t.Fatalf("restoration rejected valid stream: %v %s", err, out)
			}
			for _, service := range []string{": comment", "id: synthetic", "retry: 1000", "x-provider: retained", "ignored-field"} {
				if strings.Count(body, service) != bytes.Count(out, []byte(service)) {
					t.Errorf("service field changed: %s", service)
				}
			}
		})
	}
}

func TestAuditSSEMultilineOpaquePreserved(t *testing.T) {
	x, alias := supportedResponseExchange(t)
	a, _ := json.Marshal(alias)
	opaque := "id: synthetic\n: retain me\nevent: future_event\ndata: {\ndata: \"type\":\"future_event\",\ndata: \"text\":" + string(a) + "}\n\n"
	body := reasoningSSEFrame("message_start", `{"type":"message_start","message":{"content":[]}}`) + opaque + reasoningSSEFrame("message_stop", `{"type":"message_stop"}`)
	out, err := x.Restore([]byte(body), true)
	if err != nil || string(out) != body {
		t.Fatalf("opaque multiline frame changed/rejected: %v %s", err, out)
	}
}

func TestAuditSSEMalformedDataStillRejected(t *testing.T) {
	start := reasoningSSEFrame("message_start", `{"type":"message_start","message":{"content":[]}}`)
	end := reasoningSSEFrame("message_stop", `{"type":"message_stop"}`)
	for _, body := range []string{
		start + "event: future\ndata: {\ndata: broken}\n\n" + end,
		start + "event: error\ndata: {\ndata: \"type\":\"error\"}\n\n" + end,
		start + strings.TrimSuffix(end, "\n"),
		start + "event: error\ndata:\n\n" + end,
		start + "event: future\ndata:\n\n" + end,
		": keepalive\n\n",
	} {
		if err := checkUnmaskedTrafficSSE([]byte(body)); err == nil {
			t.Fatal("incomplete or invalid stream admitted")
		}
	}
}

func TestAuditSSEFramingByteAtATime(t *testing.T) {
	x, alias := supportedResponseExchange(t)
	a, _ := json.Marshal(alias)
	for _, newline := range []string{"\n", "\r\n", "\r"} {
		t.Run(jsonQuoteAudit(newline), func(t *testing.T) {
			opaque := "id: synthetic\n: keep\nevent: future_event\ndata: {\ndata: \"type\":\"future_event\",\ndata: \"text\":" + string(a) + "}\n\n"
			body := reasoningSSEFrame("message_start", `{"type":"message_start","message":{"content":[]}}`) + opaque + reasoningSSEFrame("message_stop", `{"type":"message_stop"}`)
			body = strings.ReplaceAll(body, "\n", newline)
			var out bytes.Buffer
			w := x.engine.NewStreamUnmasker(x.request, &out)
			for i := range len(body) {
				if _, err := w.Write([]byte{body[i]}); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if out.String() != body {
				t.Fatalf("fragmentation changed opaque bytes:\n%q\nwant %q", out.String(), body)
			}
			life := &protectedLifecycle{clock: newCompatClock(), limits: LifecycleLimits{Idle: time.Minute}}
			read, err := readProtectedBody(iotest.OneByteReader(strings.NewReader(body)), true, life)
			if err != nil {
				t.Fatal(err)
			}
			// CR dispatches a complete event immediately; a following LF only
			// completes that line ending and is outside the terminal prefix.
			want := body
			if newline == "\r\n" {
				want = strings.TrimSuffix(want, "\n")
			}
			if string(read) != want {
				t.Fatalf("wrong terminal prefix: %q want %q", read, want)
			}
			if err := checkUnmaskedTrafficSSE(read); err != nil {
				t.Fatalf("fragmented prefix rejected: %v", err)
			}
		})
	}
}

func jsonQuoteAudit(value string) string { encoded, _ := json.Marshal(value); return string(encoded) }

func TestAuditSSEInitialKnownAndOpaqueContent(t *testing.T) {
	x, alias := supportedResponseExchange(t)
	a, _ := json.Marshal(alias)
	start := reasoningSSEFrame("message_start", `{"type":"message_start","message":{"content":[],"input_transformations":[]}}`)
	known := reasoningSSEFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":`+string(a)+`,"future":{"text":`+string(a)+`}}}`) + reasoningSSEFrame("content_block_stop", `{"type":"content_block_stop","index":0}`)
	opaque := reasoningSSEFrame("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","content":[{"type":"web_search_result","title":`+string(a)+`,"encrypted_content":"opaque"}]}}`) + reasoningSSEFrame("content_block_stop", `{"type":"content_block_stop","index":1}`)
	body := start + known + opaque + reasoningSSEFrame("message_stop", `{"type":"message_stop"}`)
	out, err := x.Restore([]byte(body), true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("ResponseCanary")) || !bytes.Contains(out, []byte(opaque)) || !bytes.Contains(out, []byte(`"future":{"text":`+string(a)+`}`)) {
		t.Fatalf("initial block restoration changed opaque fields: %s", out)
	}
	// Anthropic documents message_start.content as empty: keep its shape check.
	invalid := strings.Replace(body, `"content":[]`, `"content":[{"type":"text","text":"already-open"}]`, 1)
	if err := checkUnmaskedTrafficSSE([]byte(invalid)); err == nil {
		t.Fatal("nonempty message_start content accepted")
	}
}

func TestAuditSSEMidstreamBOMIsNotAFieldPrefix(t *testing.T) {
	start := reasoningSSEFrame("message_start", `{"type":"message_start","message":{"content":[]}}`)
	falseTerminal := "\xef\xbb\xbfdata: {\"type\":\"message_stop\"}\n\n"
	body := start + falseTerminal
	if err := checkUnmaskedTrafficSSE([]byte(body)); err == nil {
		t.Fatal("midstream BOM invented a terminal event")
	}
	life := &protectedLifecycle{clock: newCompatClock(), limits: LifecycleLimits{Idle: time.Minute}}
	if _, err := readProtectedBody(strings.NewReader(body), true, life); err == nil {
		t.Fatal("lifecycle stopped on ignored midstream field")
	}
	x, _ := supportedResponseExchange(t)
	body += reasoningSSEFrame("message_stop", `{"type":"message_stop"}`)
	out, err := x.Restore([]byte(body), true)
	if err != nil || string(out) != body {
		t.Fatalf("ignored midstream BOM field changed: %v %q", err, out)
	}
}
