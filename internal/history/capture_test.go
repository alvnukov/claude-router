package history

import (
	"bytes"
	"compress/gzip"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSSE(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":10,"cache_read_input_tokens":4}}}`,
		``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
		``,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"Read","input":{}}}`,
		``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"x\"}"}}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`,
		``,
	}, "\n")
	p := ParseResponse("text/event-stream", "", []byte(sse), false)
	if p.Model != "m" || p.StopReason != "tool_use" || p.Usage["input_tokens"] != 10 || p.Usage["output_tokens"] != 7 || p.Usage["cache_read_input_tokens"] != 4 {
		t.Fatalf("meta: %+v", p)
	}
	if len(p.Blocks) != 2 || p.Blocks[0].Text != "Hello" || p.Blocks[1].Name != "Read" || !strings.Contains(p.Blocks[1].Input, `"path": "x"`) {
		t.Fatalf("blocks: %+v", p.Blocks)
	}
}

func TestParseJSONError(t *testing.T) {
	p := ParseResponse("application/json", "", []byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`), false)
	if p.Error != "overloaded_error: busy" {
		t.Fatalf("got %q", p.Error)
	}
}

func TestParseGzipResponse(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(`{"type":"message","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"hi"}],"usage":{"output_tokens":1}}`))
	zw.Close()
	p := ParseResponse("application/json", "gzip", buf.Bytes(), false)
	if p.Note != "" || p.Error != "" || len(p.Blocks) != 1 || p.Blocks[0].Text != "hi" {
		t.Fatalf("%+v", p)
	}
	p = ParseResponse("application/json", "br", []byte{3, 4, 5}, false)
	if p.Note == "" || p.Error != "" {
		t.Fatalf("br should be a note, got %+v", p)
	}
}

func TestHistoryPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.jsonl")
	st := New(2, p)
	for i := 0; i < 3; i++ {
		r := &Record{Start: time.Now(), Model: "m", Route: "local", ReqBody: []byte(`{}`)}
		st.Add(r)
		rw := NewRecorder(httptest.NewRecorder(), 1<<20)
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(200)
		_, _ = rw.Write([]byte(`{"type":"message","content":[{"type":"text","text":"hi"}]}`))
		st.Finish(r.ID, rw, nil)
	}
	st2 := New(2, p)
	got := st2.List()
	if len(got) != 2 || got[0].Resp == nil || got[0].Resp.Text() != "hi" || got[0].Seq != 2 {
		t.Fatalf("loaded %d records, first=%+v", len(got), got[0])
	}
	st2.Clear()
	if b, _ := os.ReadFile(p); len(b) != 0 {
		t.Fatal("clear did not truncate the file")
	}
}

// The recorder keeps the first status it saw; a body without WriteHeader is
// a 200, as net/http sends it.
func TestRecorderStatus(t *testing.T) {
	rw := NewRecorder(httptest.NewRecorder(), 0)
	if rw.Status() != 0 {
		t.Fatalf("status before a write = %d", rw.Status())
	}
	_, _ = rw.Write([]byte("x"))
	rw.WriteHeader(500)
	if rw.Status() != 200 {
		t.Fatalf("status = %d, want 200", rw.Status())
	}
}

func TestSessionOf(t *testing.T) {
	body := []byte(`{"model":"x","metadata":{"user_id":"{\"device_id\":\"d\",\"account_uuid\":\"a\",\"session_id\":\"s-123\"}"},"messages":[]}`)
	if got := SessionOf(body); got != "s-123" {
		t.Fatalf("session = %q", got)
	}
	for _, b := range []string{`{"model":"x","messages":[]}`, `{"metadata":{"user_id":"plain"}}`, `not json`} {
		if got := SessionOf([]byte(b)); got != "" {
			t.Fatalf("%s: session = %q, want empty", b, got)
		}
	}
}
