package privacy

import (
	"bytes"
	"slices"
)

// DropUnsignedThinking removes the assistant thinking blocks Anthropic cannot
// verify. Codex reasoning reaches the client as thinking without a signature,
// and Anthropic refuses such a block in any later request. A body without one
// comes back as the same slice; otherwise the blocks go, a message left empty
// goes with them, and everything else keeps its bytes.
//
// With thinking on, the turn in progress must begin with thinking, and a
// signature cannot be made up. So when that turn no longer begins with
// thinking, thinking is turned off for the request. Adaptive thinking carries
// no such requirement and is left alone.
func DropUnsignedThinking(body []byte) []byte {
	root, err := scanJSON(body)
	if err != nil || root.kind != '{' {
		return body
	}
	messages := root.get("messages")
	if messages == nil || messages.kind != '[' {
		return body
	}
	type shape struct {
		role   string
		result bool   // carries a tool_result, so the turn goes on
		first  string // type of the first block
	}
	var kept [][]byte
	var shapes []shape
	dropped := false
	for _, m := range messages.items {
		content := m.get("content")
		raw := body[m.start:m.end]
		var blocks []*jsonNode
		if content != nil && content.kind == '[' {
			blocks = content.items
		}
		if m.str("role") == "assistant" {
			all := len(blocks)
			blocks = slices.DeleteFunc(slices.Clone(blocks), unsignedThinking)
			if len(blocks) < all {
				dropped = true
				if len(blocks) == 0 {
					continue
				}
				parts := make([][]byte, len(blocks))
				for i, b := range blocks {
					parts[i] = body[b.start:b.end]
				}
				raw = slices.Concat(body[m.start:content.start], []byte("["), bytes.Join(parts, []byte(",")), []byte("]"), body[content.end:m.end])
			}
		}
		s := shape{role: m.str("role")}
		for i, b := range blocks {
			if i == 0 {
				s.first = b.str("type")
			}
			s.result = s.result || b.str("type") == "tool_result"
		}
		kept, shapes = append(kept, raw), append(shapes, s)
	}
	if !dropped {
		return body
	}
	edits := []rawEdit{{messages.start, messages.end, slices.Concat([]byte("["), bytes.Join(kept, []byte(",")), []byte("]"))}}
	thinking, last := root.get("thinking"), len(shapes)-1
	if thinking == nil || thinking.str("type") != "enabled" || last < 0 || shapes[last].role != "user" || !shapes[last].result {
		return applyRaw(body, edits)
	}
	turn := -1
	for i, s := range shapes {
		if s.role == "user" && !s.result {
			turn = i
		}
	}
	for _, s := range shapes[turn+1:] {
		if s.role != "assistant" {
			continue
		}
		if s.first != "thinking" && s.first != "redacted_thinking" {
			edits = append(edits, rawEdit{thinking.start, thinking.end, []byte(`{"type":"disabled"}`)})
			edits = append(edits, dropPair(root, "context_management")...)
		}
		break
	}
	return applyRaw(body, edits)
}

func unsignedThinking(n *jsonNode) bool {
	if n.kind != '{' || n.str("type") != "thinking" {
		return false
	}
	s := n.get("signature")
	return s == nil || s.kind == 'n' || s.kind == '"' && s.text == ""
}

// dropPair removes one member of an object together with the comma that
// joins it to a neighbour.
func dropPair(n *jsonNode, key string) []rawEdit {
	for i, p := range n.pairs {
		if p.key.text != key {
			continue
		}
		switch {
		case i > 0:
			return []rawEdit{{n.pairs[i-1].value.end, p.value.end, nil}}
		case len(n.pairs) > 1:
			return []rawEdit{{p.key.start, n.pairs[1].key.start, nil}}
		default:
			return []rawEdit{{p.key.start, p.value.end, nil}}
		}
	}
	return nil
}
