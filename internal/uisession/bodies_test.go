package uisession

import (
	"strconv"
	"testing"
)

func testPreview(body []byte) string { return "preview " + strconv.Itoa(len(body)) }

func TestBodiesDecodeOncePerRequest(t *testing.T) {
	first := []byte(`{"output_config":{"effort":"high"},"messages":[{"role":"user","content":"Первая"}]}`)
	other := []byte(`{"output_config":{"effort":"low"},"messages":[{"role":"user","content":"Вторая задача"}]}`)
	want := Facts{Effort: "high", Prompt: "Первая", Preview: testPreview(first)}
	wantOther := Facts{Effort: "low", Prompt: "Вторая задача", Preview: testPreview(other)}
	var c Bodies
	if got := c.Facts("req_1", first, testPreview); got != want {
		t.Fatalf("first=%+v", got)
	}
	t.Run("cached by request id", func(t *testing.T) {
		if got := c.Facts("req_1", other, testPreview); got != want {
			t.Fatalf("request decoded again: %+v", got)
		}
	})
	t.Run("sweep forgets unseen requests", func(t *testing.T) {
		c.Sweep()
		c.Sweep()
		if got := c.Facts("req_1", other, testPreview); got != wantOther {
			t.Fatalf("stale facts after sweep: %+v", got)
		}
	})
	t.Run("sweep keeps requests still seen", func(t *testing.T) {
		c.Sweep()
		c.Facts("req_1", first, testPreview)
		c.Sweep()
		if got := c.Facts("req_1", first, testPreview); got != wantOther {
			t.Fatalf("live request decoded again: %+v", got)
		}
	})
	t.Run("preview decoded once", func(t *testing.T) {
		calls := 0
		count := func([]byte) string { calls++; return "x" }
		var d Bodies
		d.Facts("req_2", first, count)
		d.Facts("req_2", first, count)
		if calls != 1 {
			t.Fatalf("preview decoded %d times", calls)
		}
	})
	t.Run("no id is not cached", func(t *testing.T) {
		c.Facts("", first, testPreview)
		if got := c.Facts("", other, testPreview); got.Effort != "low" {
			t.Fatalf("empty id cached: %+v", got)
		}
	})
}
