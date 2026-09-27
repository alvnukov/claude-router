package privacy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func streamDelta(index int, typ, text string) string {
	field := "text"
	if typ == "input_json_delta" {
		field = "partial_json"
	}
	data, _ := json.Marshal(text)
	return fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":%q,%q:%s}}\n\n", index, typ, field, data)
}
func streamText(t *testing.T, data string) string {
	t.Helper()
	var out strings.Builder
	for _, line := range strings.Split(data, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var v struct {
			Delta struct {
				Text    string `json:"text"`
				Partial string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
			t.Fatal(err)
		}
		out.WriteString(v.Delta.Text)
		out.WriteString(v.Delta.Partial)
	}
	return out.String()
}
func TestStreamHoldback(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	body := requestBody("stream", "Ромашка 10.1.2.3")
	masked, req, err := e.Mask(body)
	if err != nil {
		t.Fatal(err)
	}
	defer req.Close()
	text, _ := lookupString(masked, "system")
	for _, size := range []int{1, 7, 4096} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			var sink bytes.Buffer
			w := e.NewStreamUnmasker(req, &sink)
			stream := ""
			for _, r := range text {
				stream += streamDelta(0, "text_delta", string(r))
			}
			opaque := "event: content_block_delta\r\ndata: {\"index\":1,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"10.1.2.3\"}}\r\n\r\n"
			stream += opaque + "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
			for i := 0; i < len(stream); i += size {
				if _, err := w.Write([]byte(stream[i:min(i+size, len(stream))])); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if got := streamText(t, sink.String()); got != "Ромашка 10.1.2.3" {
				t.Fatalf("got %q", got)
			}
			if !strings.Contains(sink.String(), opaque) {
				t.Fatal("opaque frame changed")
			}
		})
	}
	t.Run("invalid-data", func(t *testing.T) {
		var sink bytes.Buffer
		w := e.NewStreamUnmasker(req, &sink)
		if _, err := w.Write([]byte("event: content_block_delta\ndata: nope\n\n")); err == nil {
			t.Fatal("bad data accepted")
		}
		if !strings.Contains(sink.String(), "event: error") {
			t.Fatal("missing error frame")
		}
	})
}

func TestStreamToolJSONIsAtomic(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	masked, req := mustMask(t, e, []byte(`{"messages":[{"content":[{"type":"tool_use","input":{"password":"x'\\y\"z"}}]}]}`))
	pseudo, _ := lookupString(masked, "messages", "0", "content", "0", "input", "password")
	value, _ := json.Marshal(pseudo)
	input := `{"password":` + string(value) + `}`
	var sink bytes.Buffer
	w := e.NewStreamUnmasker(req, &sink)
	for _, r := range input {
		if _, err := w.Write([]byte(streamDelta(0, "input_json_delta", string(r)))); err != nil {
			t.Fatal(err)
		}
	}
	if streamText(t, sink.String()) != "" {
		t.Fatal("partial tool input released before validation")
	}
	if _, err := w.Write([]byte("event: content_block_stop\ndata: {\"index\":0}\n\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(streamText(t, sink.String())), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["password"] != "x'\\y\"z" {
		t.Fatal("escaped secret changed")
	}
	t.Run("invalid-json", func(t *testing.T) {
		sink.Reset()
		w := e.NewStreamUnmasker(req, &sink)
		if _, err := w.Write([]byte(streamDelta(0, "input_json_delta", `{"password":"`+pseudo))); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err == nil {
			t.Fatal("unfinished tool input silently repaired or released")
		}
		if streamText(t, sink.String()) != "" {
			t.Fatal("unsafe partial input released")
		}
	})
}

func TestStreamUnicodeEscapedToolInput(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	masked, req := mustMask(t, e, requestBody("unicode-stream", "Ромашка"))
	pseudo, _ := lookupString(masked, "system")
	var escaped strings.Builder
	for _, r := range pseudo {
		fmt.Fprintf(&escaped, `\u%04x`, r)
	}
	var sink bytes.Buffer
	w := e.NewStreamUnmasker(req, &sink)
	for _, r := range `{"company":"` + escaped.String() + `"}` {
		if _, err := w.Write([]byte(streamDelta(0, "input_json_delta", string(r)))); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(streamText(t, sink.String())), &decoded); err != nil || decoded["company"] != "Ромашка" {
		t.Fatal("escaped pseudonym not restored", err)
	}
}
