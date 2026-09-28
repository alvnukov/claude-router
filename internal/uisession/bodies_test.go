package uisession

import "testing"

func TestBodiesDecodeOncePerRequest(t *testing.T) {
	first := []byte(`{"output_config":{"effort":"high"},"messages":[{"role":"user","content":"Первая"}]}`)
	other := []byte(`{"output_config":{"effort":"low"},"messages":[{"role":"user","content":"Вторая"}]}`)
	want := Facts{Effort: "high", Prompt: "Первая"}
	var c Bodies
	if got := c.Facts("req_1", first); got != want {
		t.Fatalf("first=%+v", got)
	}
	t.Run("cached by request id", func(t *testing.T) {
		if got := c.Facts("req_1", other); got != want {
			t.Fatalf("request decoded again: %+v", got)
		}
	})
	t.Run("sweep forgets unseen requests", func(t *testing.T) {
		c.Sweep()
		c.Sweep()
		if got := c.Facts("req_1", other); got != (Facts{Effort: "low", Prompt: "Вторая"}) {
			t.Fatalf("stale facts after sweep: %+v", got)
		}
	})
	t.Run("sweep keeps requests still seen", func(t *testing.T) {
		c.Sweep()
		c.Facts("req_1", first)
		c.Sweep()
		if got := c.Facts("req_1", first); got != (Facts{Effort: "low", Prompt: "Вторая"}) {
			t.Fatalf("live request decoded again: %+v", got)
		}
	})
	t.Run("no id is not cached", func(t *testing.T) {
		c.Facts("", first)
		if got := c.Facts("", other); got.Effort != "low" {
			t.Fatalf("empty id cached: %+v", got)
		}
	})
}
