package main

import (
	"strings"
	"testing"
)

func call(id, name, args string) openaiToolCall {
	var c openaiToolCall
	c.ID, c.Type = id, "function"
	c.Function.Name, c.Function.Arguments = name, args
	return c
}

// checkValid asserts the invariant the endpoint actually enforces: every `tool`
// message answers a tool_call that is still in the request.
func checkValid(t *testing.T, r openaiRequest) {
	t.Helper()
	seen := map[string]bool{}
	for _, m := range r.Messages {
		for _, tc := range m.ToolCalls {
			seen[tc.ID] = true
		}
		if m.Role == "tool" && !seen[m.ToolCallID] {
			t.Fatalf("orphan tool reply %q", m.ToolCallID)
		}
	}
}

func convo(turns int, resultSize int) openaiRequest {
	r := openaiRequest{Model: "m", Messages: []openaiMsg{{Role: "system", Content: "system prompt"}}}
	for i := 0; i < turns; i++ {
		id := string(rune('a' + i%26))
		r.Messages = append(r.Messages,
			openaiMsg{Role: "user", Content: "please read the file"},
			openaiMsg{Role: "assistant", ToolCalls: []openaiToolCall{call(id, "Read", `{"path":"f"}`)}},
			openaiMsg{Role: "tool", ToolCallID: id, Content: strings.Repeat("A", resultSize)},
		)
	}
	return r
}

func TestUnderBudgetIsUntouched(t *testing.T) {
	r := convo(2, 100)
	before, after, notes := fitToBudget(&r, 1_000_000)
	if before != after || notes != nil {
		t.Fatalf("touched a request that fits: %d -> %d %v", before, after, notes)
	}
}

func TestHugeToolResultIsElidedNotDropped(t *testing.T) {
	r := convo(1, 50_000)
	n := len(r.Messages)
	before, after, _ := fitToBudget(&r, 10_000)
	if after > 10_000 {
		t.Fatalf("still over budget: %d -> %d", before, after)
	}
	if len(r.Messages) != n {
		t.Fatalf("dropped messages when eliding would do: %d -> %d", n, len(r.Messages))
	}
	checkValid(t, r)
}

func TestLongConversationDropsOldestAndStaysValid(t *testing.T) {
	r := convo(40, 4_000)
	last := r.Messages[len(r.Messages)-1]
	before, after, notes := fitToBudget(&r, 30_000)
	if after > 30_000 {
		t.Fatalf("still over budget: %d -> %d (%v)", before, after, notes)
	}
	if r.Messages[0].Role != "system" || !strings.HasPrefix(r.Messages[0].Content.(string), "system prompt") {
		t.Fatalf("system prompt lost: %+v", r.Messages[0])
	}
	if got := r.Messages[len(r.Messages)-1]; got.ToolCallID != last.ToolCallID {
		t.Fatalf("newest message not preserved")
	}
	checkValid(t, r)
}

// The cut must run past the tail boundary rather than leave a tool reply whose
// assistant call it just dropped.
func TestCutNeverOrphansToolReply(t *testing.T) {
	for _, turns := range []int{3, 5, 8, 13, 21} {
		r := convo(turns, 9_000)
		fitToBudget(&r, 12_000)
		checkValid(t, r)
	}
}

func TestToolCallArgumentsStayValidJSON(t *testing.T) {
	r := convo(1, 10)
	r.Messages[2].ToolCalls[0].Function.Arguments = `{"content":"` + strings.Repeat("x", 60_000) + `"}`
	fitToBudget(&r, 5_000)
	args := r.Messages[2].ToolCalls[0].Function.Arguments
	if !strings.HasPrefix(args, `{"_router_elided_chars":`) || !strings.HasSuffix(args, "}") {
		t.Fatalf("arguments not replaced with valid JSON: %.60s", args)
	}
}

func TestToolDefinitionsAreNeverTruncated(t *testing.T) {
	r := convo(1, 100)
	var tool openaiTool
	tool.Type = "function"
	tool.Function.Name = "Edit"
	tool.Function.Parameters = []byte(`{"x":"` + strings.Repeat("s", 20_000) + `"}`)
	r.Tools = append(r.Tools, tool)
	want := string(tool.Function.Parameters)
	fitToBudget(&r, 1_000)
	if string(r.Tools[0].Function.Parameters) != want {
		t.Fatalf("tool schema was mangled: %d -> %d chars", len(want), len(r.Tools[0].Function.Parameters))
	}
}

// The permission classifier sends the transcript as a couple of huge blocks:
// nothing to drop, nothing under the tail guard. Those must land near the
// budget, not be gutted to a 1.3k stub (seen 2026-09-08: 213074 -> 1304).
func TestFewHugeBlocksLandNearBudget(t *testing.T) {
	r := openaiRequest{Model: "m", Messages: []openaiMsg{
		{Role: "system", Content: strings.Repeat("S", 20_000)},
		{Role: "user", Content: strings.Repeat("U", 200_000)},
	}}
	before, after, notes := fitToBudget(&r, 180_000)
	if after > 180_000 {
		t.Fatalf("still over budget: %d -> %d (%v)", before, after, notes)
	}
	if after < 180_000/4 {
		t.Fatalf("gutted the request: %d -> %d (%v)", before, after, notes)
	}
}

func TestBudgetZeroDisables(t *testing.T) {
	r := convo(30, 20_000)
	n := len(r.Messages)
	if _, _, notes := fitToBudget(&r, 0); notes != nil || len(r.Messages) != n {
		t.Fatalf("trimmed with the guard disabled")
	}
}
