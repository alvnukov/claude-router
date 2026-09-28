package privacy

import (
	"bytes"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

func reviewPolicy(t *testing.T, rules string) *Policy {
	t.Helper()
	home := t.TempDir()
	runtimeConfig(t, home, rules)
	p, err := NewRuntime(home).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestTrafficReviewFalseSourcesAndNumericIdentifiers(t *testing.T) {
	p := reviewPolicy(t, `{"entries":[{"kind":"org","forms":["PrivateCanary","123456789"]}]}`)
	for _, body := range []string{
		`{"system":"safe","output_config":{"type":"image","opaque":"PrivateCanary"}}`,
		`{"system":"safe","tools":[{"name":"safe","input_schema":{"type":"image","pattern":"PrivateCanary"}}]}`,
		`{"system":"safe","tools":[{"name":"safe","input_schema":{"type":"object","x-custom":{"type":"document","source":"PrivateCanary"}}}]}`,
		`{"system":"safe","max_tokens":123456789}`,
		`{"system":"safe","tools":[{"name":"safe","input_schema":{"type":"object","maxLength":123456789}}]}`,
	} {
		x, wire, err := p.Prepare(Target{}, []byte(body))
		if x != nil {
			x.Close()
		}
		if err != nil || x == nil || !bytes.Equal(wire, []byte(body)) {
			t.Errorf("opaque or numeric input changed: %s (%v)", wire, err)
		}
	}
	imageBody := []byte(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"PrivateCanary"}}]}]}`)
	x, wire, err := p.Prepare(Target{}, imageBody)
	if err != nil {
		t.Fatal("opaque image rejected", err)
	}
	defer x.Close()
	if !bytes.Equal(wire, imageBody) {
		t.Fatal("opaque image changed")
	}
}
func TestTrafficReviewSSEControlsAndBlockTypes(t *testing.T) {
	p := reviewPolicy(t, `{}`)
	frames := func(lines ...string) []byte {
		var b strings.Builder
		for _, line := range lines {
			b.WriteString("data: " + line + "\n\n")
		}
		return []byte(b.String())
	}
	start := `{"type":"message_start","message":{"type":"message","content":[]}}`
	stop := `{"type":"message_stop"}`
	for _, tc := range []struct {
		name   string
		stream []byte
		reject bool
	}{
		{"stop-sequence", frames(start, `{"type":"message_delta","delta":{"stop_sequence":"<secret:credential:deadbeef>"}}`, stop), false},
		{"terminal-extra-field", frames(start, `{"type":"message_stop","delta":{"text":"<secret:credential:deadbeef>"}}`), false},
		{"text-extra-field", frames(start, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"safe","partial_json":"<secret:credential:deadbeef>"}}`, `{"type":"content_block_stop","index":0}`, stop), false},
		{"service-metadata", frames(`{"type":"message_start","message":{"type":"message","content":[],"metadata":"<secret:credential:deadbeef>"}}`, stop), false},
		{"wrong-known-delta-type", frames(start, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"1","name":"run","input":{}}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"not tool JSON"}}`, `{"type":"content_block_stop","index":0}`, stop), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, _, err := p.Prepare(Target{}, []byte(`{"system":"password=SecretCanary123"}`))
			if err != nil {
				t.Fatal(err)
			}
			defer x.Close()
			out, err := x.Restore(tc.stream, true)
			if tc.reject {
				if err == nil || len(out) != 0 {
					t.Fatal("wrong known delta type released")
				}
			} else if tc.name == "text-extra-field" {
				const opaque = `"partial_json":"<secret:credential:deadbeef>"`
				if err != nil || bytes.Count(out, []byte(opaque)) != 1 {
					t.Fatal("opaque delta field changed", err)
				}
				var text strings.Builder
				for _, frame := range bytes.Split(out, []byte("\n\n")) {
					event, node, _ := protectedFrameEvent(frame)
					if event == "content_block_delta" && node != nil && node.get("delta") != nil {
						text.WriteString(node.get("delta").str("text"))
					}
				}
				if text.String() != "safe" {
					t.Fatal("supported text changed")
				}
			} else if err != nil || !bytes.Equal(out, tc.stream) {
				t.Fatalf("opaque/service SSE fields changed: %v\n%s", err, out)
			}
		})
	}

}
func TestTrafficReviewExpansionBoundBeforeAllocation(t *testing.T) {
	p := reviewPolicy(t, `{}`)
	body, _ := json.Marshal(map[string]string{"system": "password=" + strings.Repeat("!", 64<<10)})
	x, wire, err := p.Prepare(Target{}, body)
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	var obj map[string]string
	_ = json.Unmarshal(wire, &obj)
	alias := strings.TrimPrefix(obj["system"], "password=")
	response, _ := json.Marshal(map[string]string{"content": strings.Repeat(alias+" ", 300)})
	delta, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": strings.Repeat(alias+" ", 300)}})
	sse := []byte("data: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\ndata: " + string(delta) + "\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_stop\"}\n\n")
	if err := x.checkTrafficSSE(sse); err != nil {
		t.Fatal("expansion fixture has invalid SSE lifecycle", err)
	}
	for _, test := range []struct {
		name   string
		body   []byte
		stream bool
	}{{"json", response, false}, {"sse", sse, true}} {
		t.Run(test.name, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			out, err := x.Restore(test.body, test.stream)
			runtime.ReadMemStats(&after)
			if err == nil || out != nil {
				t.Fatal("expanded response accepted")
			}
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 32<<20 {
				t.Fatalf("limit checked after expansion: %d bytes allocated for %d byte response", allocated, len(test.body))
			}
		})
	}
}
