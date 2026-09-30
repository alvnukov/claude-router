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
func DropUnsignedThinking(body []byte) []byte {
	root, err := scanJSON(body)
	if err != nil || root.kind != '{' {
		return body
	}
	messages := root.get("messages")
	if messages == nil || messages.kind != '[' {
		return body
	}
	var kept [][]byte
	dropped := false
	for _, m := range messages.items {
		content := m.get("content")
		raw := body[m.start:m.end]
		if m.str("role") == "assistant" && content != nil && content.kind == '[' {
			blocks := slices.DeleteFunc(slices.Clone(content.items), unsignedThinking)
			if len(blocks) < len(content.items) {
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
		kept = append(kept, raw)
	}
	if !dropped {
		return body
	}
	return applyRaw(body, []rawEdit{{messages.start, messages.end, slices.Concat([]byte("["), bytes.Join(kept, []byte(",")), []byte("]"))}})
}

func unsignedThinking(n *jsonNode) bool {
	if n.kind != '{' || n.str("type") != "thinking" {
		return false
	}
	s := n.get("signature")
	return s == nil || s.kind == 'n' || s.kind == '"' && s.text == ""
}
