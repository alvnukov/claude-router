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
		x, _, err := p.Prepare(Target{}, []byte(body))
		if x != nil {
			x.Close()
		}
		if err == nil {
			t.Errorf("uncovered input accepted: %s", body)
		}
	}
	x, wire, err := p.Prepare(Target{}, []byte(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"PrivateCanary"}}]}]}`))
	if err != nil {
		t.Fatal("real withheld image rejected", err)
	}
	defer x.Close()
	if bytes.Contains(wire, []byte("PrivateCanary")) {
		t.Fatal("real image not withheld")
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
	stop := `{"type":"message_stop"}`
	for _, stream := range [][]byte{
		frames(`{"type":"message_delta","delta":{"stop_sequence":"<secret:credential:deadbeef>"}}`, stop),
		frames(`{"type":"message_stop","delta":{"text":"<secret:credential:deadbeef>"}}`),
		frames(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"safe","partial_json":"<secret:credential:deadbeef>"}}`, `{"type":"content_block_stop","index":0}`, stop),
		frames(`{"type":"message_start","message":{"type":"message","content":[],"metadata":"<secret:credential:deadbeef>"}}`, stop),
		frames(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"1","name":"run","input":{}}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"not tool JSON"}}`, `{"type":"content_block_stop","index":0}`, stop),
	} {
		x, _, err := p.Prepare(Target{}, []byte(`{"system":"password=SecretCanary123"}`))
		if err != nil {
			t.Fatal(err)
		}
		out, err := x.Restore(stream, true)
		x.Close()
		if err == nil || len(out) > 0 {
			t.Error("malformed SSE released")
		}
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
	sse := []byte("data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\ndata: " + string(delta) + "\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_stop\"}\n\n")
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
