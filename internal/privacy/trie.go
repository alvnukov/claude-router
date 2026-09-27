package privacy

import (
	"unicode"
	"unicode/utf8"
)

type matchValue struct {
	real    string
	pseudo  string
	kind    Kind
	foreign bool
	secret  bool
}
type trieNode struct {
	next  map[rune]*trieNode
	value *matchValue
}
type textMatch struct {
	start, end int
	value      matchValue
}

func (t *trieNode) add(key string, value matchValue) {
	n := t
	for _, r := range key {
		r = unicode.ToLower(r)
		if n.next == nil {
			n.next = make(map[rune]*trieNode)
		}
		child := n.next[r]
		if child == nil {
			child = &trieNode{}
			n.next[r] = child
		}
		n = child
	}
	value.pseudo = key
	n.value = &value
}
func (t *trieNode) matches(text string) []textMatch {
	var out []textMatch
	for start := 0; start < len(text); {
		n := t
		end := start
		best := textMatch{start: start}
		for end < len(text) {
			r, size := utf8.DecodeRuneInString(text[end:])
			n = n.next[unicode.ToLower(r)]
			if n == nil {
				break
			}
			end += size
			if n.value != nil {
				best.end = end
				best.value = *n.value
			}
		}
		if best.end > start {
			out = append(out, best)
			start = best.end
		} else {
			_, size := utf8.DecodeRuneInString(text[start:])
			start += size
		}
	}
	return out
}
func (t *trieNode) suffixPrefix(text string) int {
	for start := 0; start < len(text); {
		n := t
		end := start
		for end < len(text) {
			r, size := utf8.DecodeRuneInString(text[end:])
			n = n.next[unicode.ToLower(r)]
			if n == nil {
				break
			}
			end += size
		}
		if n != nil && len(n.next) > 0 {
			return start
		}
		_, size := utf8.DecodeRuneInString(text[start:])
		start += size
	}
	return len(text)
}
