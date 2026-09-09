package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Claude Code sizes a conversation for the cloud model and hands the router
// whatever it built; it has no per-model window, so it cannot size it for the
// local endpoint. Refusing an oversized prompt moves the failure to the user and
// puts it somewhere they cannot fix: /compact is itself a request carrying the
// same conversation, so a session over the limit cannot compact its way back
// under it. The router reduces the request instead, cheapest loss first --
// oversized individual blocks are elided before whole turns are dropped, and
// the newest turns are given up last.
const (
	// How many trailing messages stay intact while there is any other way to
	// save room. This is the live turn plus its immediate context.
	trimKeepTail = 6

	trimNote     = "\n[router: %d earlier message(s) omitted to fit the local model budget]"
	elideNote    = "\n…[router elided %d chars]…\n"
	elidedImage  = "[router: image elided to fit the local model budget]"
	elidedArgs   = `{"_router_elided_chars":%d}`
	minElideGain = 64 // eliding less than this costs more in markers than it saves
)

// fitToBudget shrinks r in place until it fits budget, or until nothing is left
// to give. It reports the size before and after and what it had to do.
func fitToBudget(r *openaiRequest, budget int) (before, after int, notes []string) {
	before = inputChars(*r)
	if budget <= 0 || before <= budget {
		return before, before, nil
	}

	// 1. Elide oversized blocks, tightening the cap each pass and widening the
	//    scope only once the gentler passes are exhausted. A single huge tool
	//    result is the usual cause of an overrun, and cutting its middle out
	//    costs far less than losing whole turns. `spare` is how many trailing
	//    messages are held back; the live turn is the last thing to be touched.
	for _, step := range []struct{ cap, spare int }{
		{8000, trimKeepTail}, {2000, trimKeepTail}, {500, trimKeepTail},
		{8000, 1}, {2000, 1}, {500, 1},
	} {
		if n := elideRange(r, 0, len(r.Messages)-step.spare, step.cap, false); n > 0 {
			notes = append(notes, fmt.Sprintf("elided %d block(s) over %d chars", n, step.cap))
		}
		if after = inputChars(*r); after <= budget {
			return before, after, notes
		}
	}

	// 2. Drop the oldest non-system messages, as few as will do.
	if n := dropMiddle(r, budget); n > 0 {
		notes = append(notes, fmt.Sprintf("dropped %d oldest message(s)", n))
	}
	if after = inputChars(*r); after <= budget {
		return before, after, notes
	}

	// 3. Nothing is sacred any more: the tail and the system prompt included.
	//    Still cut as little as fits. A request of a few huge blocks -- the
	//    permission classifier sends the whole transcript as one -- gets here
	//    untouched by the passes above, and must come out near the budget, not
	//    gutted to a stub the model cannot judge.
	for cap := budget / 2; cap >= 400; cap /= 2 {
		if n := elideRange(r, 0, len(r.Messages), cap, true); n > 0 {
			notes = append(notes, fmt.Sprintf("hard-elided %d block(s) to %d chars", n, cap))
		}
		if after = inputChars(*r); after <= budget {
			return before, after, notes
		}
	}
	if n := elideRange(r, 0, len(r.Messages), 400, true); n > 0 {
		notes = append(notes, fmt.Sprintf("hard-elided %d remaining block(s)", n))
	}
	after = inputChars(*r)
	if after > budget {
		notes = append(notes, fmt.Sprintf("still %d over: tool definitions alone exceed the budget", after-budget))
	}
	return before, after, notes
}

// elideRange shrinks message contents in [from,to) to cap. Tool definitions are
// never touched -- truncating a schema breaks tool calling outright, which is a
// worse failure than an oversized prompt. System messages are spared unless
// withSystem says otherwise.
func elideRange(r *openaiRequest, from, to, cap int, withSystem bool) int {
	if from < 0 {
		from = 0
	}
	if to > len(r.Messages) {
		to = len(r.Messages)
	}
	n := 0
	for i := from; i < to; i++ {
		if r.Messages[i].Role == "system" && !withSystem {
			continue
		}
		n += shrinkMsg(&r.Messages[i], cap)
	}
	return n
}

func shrinkMsg(m *openaiMsg, cap int) int {
	n := 0
	switch c := m.Content.(type) {
	case string:
		if s, ok := elide(c, cap); ok {
			m.Content = s
			n++
		}
	case []map[string]any:
		for i, part := range c {
			switch part["type"] {
			case "text":
				if s, ok := part["text"].(string); ok {
					if e, done := elide(s, cap); done {
						c[i]["text"] = e
						n++
					}
				}
			case "image_url":
				// Base64 payloads cannot be cut in the middle and stay decodable,
				// so an oversized image goes entirely.
				if imageChars(part) > cap {
					c[i] = map[string]any{"type": "text", "text": elidedImage}
					n++
				}
			}
		}
	}
	for i := range m.ToolCalls {
		args := m.ToolCalls[i].Function.Arguments
		// Arguments must stay parseable JSON, so this replaces rather than cuts.
		if len(args) > cap {
			m.ToolCalls[i].Function.Arguments = fmt.Sprintf(elidedArgs, len(args))
			n++
		}
	}
	return n
}

// elide cuts the middle out of s, keeping the head and a smaller tail: the head
// carries what the content is, the tail carries how it ended.
func elide(s string, cap int) (string, bool) {
	if cap < 32 || len(s) <= cap+minElideGain {
		return s, false
	}
	head := cap * 2 / 3
	tail := cap - head
	return s[:head] + fmt.Sprintf(elideNote, len(s)-cap) + s[len(s)-tail:], true
}

func imageChars(part map[string]any) int {
	u, _ := part["image_url"].(map[string]any)
	s, _ := u["url"].(string)
	return len(s)
}

// dropMiddle removes the oldest non-system messages until the request fits,
// then keeps going just far enough to leave no tool reply whose call it dropped.
func dropMiddle(r *openaiRequest, budget int) int {
	msgs := r.Messages
	start := 0
	for start < len(msgs) && msgs[start].Role == "system" {
		start++
	}
	limit := len(msgs) - trimKeepTail
	if limit <= start {
		return 0
	}

	total := inputChars(*r)
	cut := start
	for cut < limit && total > budget {
		total -= msgChars(msgs[cut])
		cut++
	}
	// A `tool` message whose assistant tool_call is gone is an orphan the
	// endpoint will reject, so the cut runs on past the tail boundary if it must.
	for cut < len(msgs) && msgs[cut].Role == "tool" {
		total -= msgChars(msgs[cut])
		cut++
	}
	if cut == start {
		return 0
	}

	kept := make([]openaiMsg, 0, len(msgs)-(cut-start)+1)
	kept = append(kept, msgs[:start]...)
	kept = append(kept, msgs[cut:]...)
	r.Messages = kept
	noteDrop(r, start, cut-start)
	return cut - start
}

// noteDrop tells the model the history is not contiguous. It rides on the
// system message where there is one, rather than inventing a turn.
func noteDrop(r *openaiRequest, at, n int) {
	note := fmt.Sprintf(trimNote, n)
	if at > 0 {
		if s, ok := r.Messages[at-1].Content.(string); ok {
			r.Messages[at-1].Content = s + note
			return
		}
	}
	msgs := make([]openaiMsg, 0, len(r.Messages)+1)
	msgs = append(msgs, r.Messages[:at]...)
	msgs = append(msgs, openaiMsg{Role: "user", Content: strings.TrimSpace(note)})
	msgs = append(msgs, r.Messages[at:]...)
	r.Messages = msgs
}

// msgChars accounts for one message the same way inputChars accounts for all.
func msgChars(m openaiMsg) int {
	n := 0
	switch c := m.Content.(type) {
	case string:
		n += len(c)
	default:
		if b, err := json.Marshal(c); err == nil {
			n += len(b)
		}
	}
	for _, tc := range m.ToolCalls {
		n += len(tc.Function.Name) + len(tc.Function.Arguments)
	}
	return n
}
