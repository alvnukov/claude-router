package privacy

import (
	"encoding/json"
	"errors"
	"strconv"
)

// jsonNode indexes the original bytes. Numbers and opaque fields are never
// decoded or reserialized. Object order and escapes survive. Duplicate keys are rejected because
// different JSON consumers can otherwise disagree on the effective value.
type jsonNode struct {
	start, end int
	kind       byte
	text       string
	pairs      []jsonPair
	items      []*jsonNode
}
type jsonPair struct{ key, value *jsonNode }
type jsonScanner struct {
	body []byte
	pos  int
}

func scanJSON(body []byte) (*jsonNode, error) {
	if len(body) > 32<<20 {
		return nil, errors.New("privacy: JSON body exceeds 32 MiB")
	}
	if !json.Valid(body) {
		return nil, errors.New("privacy: invalid JSON")
	}
	s := jsonScanner{body: body}
	return s.value(0)
}
func (s *jsonScanner) space() {
	for s.pos < len(s.body) {
		switch s.body[s.pos] {
		case ' ', '\t', '\n', '\r':
			s.pos++
		default:
			return
		}
	}
}
func (s *jsonScanner) value(depth int) (*jsonNode, error) {
	if depth > 256 {
		return nil, errors.New("privacy: JSON nesting exceeds 256")
	}
	s.space()
	n := &jsonNode{start: s.pos, kind: s.body[s.pos]}
	s.pos++
	switch n.kind {
	case '"':
		for s.body[s.pos] != '"' {
			if s.body[s.pos] == '\\' {
				s.pos++
			}
			s.pos++
		}
		s.pos++
		if err := json.Unmarshal(s.body[n.start:s.pos], &n.text); err != nil {
			return nil, err
		}
	case '{':
		seen := make(map[string]bool)
		s.space()
		for s.body[s.pos] != '}' {
			k, err := s.value(depth + 1)
			if err != nil {
				return nil, err
			}
			s.space()
			s.pos++
			v, err := s.value(depth + 1)
			if err != nil {
				return nil, err
			}
			if seen[k.text] {
				return nil, errors.New("privacy: duplicate JSON object key")
			}
			seen[k.text] = true
			n.pairs = append(n.pairs, jsonPair{k, v})
			s.space()
			if s.body[s.pos] != ',' {
				break
			}
			s.pos++
			s.space()
		}
		s.pos++
	case '[':
		s.space()
		for s.body[s.pos] != ']' {
			v, err := s.value(depth + 1)
			if err != nil {
				return nil, err
			}
			n.items = append(n.items, v)
			s.space()
			if s.body[s.pos] != ',' {
				break
			}
			s.pos++
			s.space()
		}
		s.pos++
	default:
		for s.pos < len(s.body) {
			b := s.body[s.pos]
			if b == ',' || b == ']' || b == '}' || b == ' ' || b == '\t' || b == '\n' || b == '\r' {
				break
			}
			s.pos++
		}
	}
	n.end = s.pos
	return n, nil
}
func (n *jsonNode) get(name string) *jsonNode {
	for _, p := range n.pairs {
		if p.key.text == name {
			return p.value
		}
	}
	return nil
}
func (n *jsonNode) str(name string) string {
	v := n.get(name)
	if v != nil && v.kind == '"' {
		return v.text
	}
	return ""
}
func lookupString(body []byte, path ...string) (string, bool) {
	n, err := scanJSON(body)
	if err != nil {
		return "", false
	}
	for _, p := range path {
		if n == nil {
			return "", false
		}
		if n.kind == '[' {
			i, e := strconv.Atoi(p)
			if e != nil || i < 0 || i >= len(n.items) {
				return "", false
			}
			n = n.items[i]
		} else {
			n = n.get(p)
		}
	}
	if n == nil || n.kind != '"' {
		return "", false
	}
	return n.text, true
}
