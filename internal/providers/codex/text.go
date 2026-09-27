package codex

import (
	"errors"
	"strings"
)

// TextPart is a completed message content part from the Responses stream.
type TextPart struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

// TextStream reconciles incremental text with the authoritative completed item.
// It returns only missing suffixes, so completed items do not duplicate deltas.
type TextStream struct {
	parts map[[2]int]*strings.Builder
}

func (s *TextStream) Delta(item, part int, text string) {
	if s.parts == nil {
		s.parts = make(map[[2]int]*strings.Builder)
	}
	key := [2]int{item, part}
	if s.parts[key] == nil {
		s.parts[key] = new(strings.Builder)
	}
	s.parts[key].WriteString(text)
}

func (s *TextStream) Done(item int, parts []TextPart) (string, error) {
	var rest strings.Builder
	for i, part := range parts {
		var text string
		switch part.Type {
		case "output_text":
			text = part.Text
		case "refusal":
			text = part.Refusal
		default:
			continue
		}
		var prefix string
		if prior := s.parts[[2]int{item, i}]; prior != nil {
			prefix = prior.String()
		}
		if !strings.HasPrefix(text, prefix) {
			return "", errors.New("Codex message text changed during streaming")
		}
		suffix := text[len(prefix):]
		rest.WriteString(suffix)
		s.Delta(item, i, suffix)
	}
	return rest.String(), nil
}
