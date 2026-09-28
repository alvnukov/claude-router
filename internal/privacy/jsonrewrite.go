package privacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

type fieldKind struct {
	nodeStart int
	path      string
	system    bool
	property  string
	toolInput bool
	objectKey bool
	rawSpan   func(int, int) []byte
}
type textEdit struct {
	start, end int
	value      string
	raw        []byte
}
type textFunc func(string, fieldKind) ([]textEdit, error)
type rawEdit struct {
	start, end int
	data       []byte
}
type jsonRewriter struct {
	supportedOnly bool
	body          []byte
	f             textFunc
	edits         []rawEdit
	source        func()
	record        func([]byte, []byte) error
}

func rewriteRequest(body []byte, f textFunc) ([]byte, error)  { return rewriteJSON(body, f, nil) }
func rewriteResponse(body []byte, f textFunc) ([]byte, error) { return rewriteJSON(body, f, nil) }
func rewriteJSON(body []byte, f textFunc, source func()) ([]byte, error) {
	return rewriteRecorded(body, f, source, nil)
}
func rewriteRecorded(body []byte, f textFunc, source func(), record func([]byte, []byte) error) ([]byte, error) {
	return rewriteRecordedMode(body, f, source, record, false)
}

// supportedOnly limits edits to known text positions and preserves opaque data.
func rewriteRecordedMode(body []byte, f textFunc, source func(), record func([]byte, []byte) error, supportedOnly bool) ([]byte, error) {
	if supportedOnly && !utf8.Valid(body) {
		return nil, errors.New("privacy: invalid UTF-8")
	}
	n, err := scanJSON(body)
	if err != nil {
		return nil, err
	}
	if n.kind != '{' {
		return nil, errors.New("privacy: expected JSON object")
	}
	w := jsonRewriter{body: body, f: f, source: source, record: record, supportedOnly: supportedOnly}
	for _, p := range n.pairs {
		switch p.key.text {
		case "system":
			err = w.content(p.value, "system", true)
		case "messages":
			for i, v := range p.value.items {
				for _, c := range v.pairs {
					if c.key.text == "content" {
						err = w.content(c.value, fmt.Sprintf("messages[%d].content", i), false)
						if err != nil {
							break
						}
					}
				}
				if err != nil {
					break
				}
			}
		case "content":
			err = w.content(p.value, "content", false)
		case "tools":
			for i, v := range p.value.items {
				for _, c := range v.pairs {
					path := fmt.Sprintf("tools[%d].%s", i, c.key.text)
					switch c.key.text {
					case "description":
						err = w.literal(c.value, fieldKind{path: path})
					case "input_schema":
						err = w.schema(c.value, path)
					}
					if err != nil {
						break
					}
				}
				if err != nil {
					break
				}
			}
		}
		if err != nil {
			return nil, err
		}
	}
	out := applyRaw(body, w.edits)
	if _, err := scanJSON(out); err != nil {
		return nil, err
	}
	return out, nil
}
func applyRaw(body []byte, edits []rawEdit) []byte {
	slices.SortFunc(edits, func(a, b rawEdit) int { return a.start - b.start })
	if len(edits) == 0 {
		return bytes.Clone(body)
	}
	var out bytes.Buffer
	out.Grow(len(body))
	pos := 0
	for _, e := range edits {
		out.Write(body[pos:e.start])
		out.Write(e.data)
		pos = e.end
	}
	out.Write(body[pos:])
	return out.Bytes()
}
func (w *jsonRewriter) content(n *jsonNode, path string, system bool) error {
	if n.kind == '"' {
		return w.literal(n, fieldKind{path: path, system: system})
	}
	if n.kind == '[' {
		for i, v := range n.items {
			if err := w.block(v, fmt.Sprintf("%s[%d]", path, i), system); err != nil {
				return err
			}
		}
	}
	return nil
}
func (w *jsonRewriter) block(n *jsonNode, path string, system bool) error {
	typ := n.str("type")
	switch typ {
	case "thinking", "redacted_thinking", "image", "document", "text", "tool_use", "tool_result":
	default:
		if w.supportedOnly {
			return nil
		}
		return &RejectError{Reason: "unsupported content block type"}
	}
	if typ == "thinking" || typ == "redacted_thinking" {
		return nil
	}
	if (typ == "image" || typ == "document") && n.get("source") != nil {
		if w.source != nil && !w.supportedOnly {
			w.edits = append(w.edits, rawEdit{n.start, n.end, []byte(`{"type":"text","text":"[privacy: изображение или документ не отправлен — профиль privacy: on]"}`)})
			w.source()
		}
		return nil
	}
	for _, p := range n.pairs {
		next := path + "." + p.key.text
		switch {
		case typ == "text" && p.key.text == "text":
			if err := w.literal(p.value, fieldKind{path: next, system: system}); err != nil {
				return err
			}
		case typ == "tool_use" && p.key.text == "input":
			if err := w.allStrings(p.value, next, true); err != nil {
				return err
			}
		case typ == "tool_result" && p.key.text == "content":
			if err := w.content(p.value, next, false); err != nil {
				return err
			}
		}
	}
	return nil
}
func (w *jsonRewriter) allStrings(n *jsonNode, path string, tool bool) error {
	if n.kind != '{' && n.kind != '[' && n.kind != '"' {
		if w.supportedOnly {
			return nil
		}
		edits, err := w.f(string(w.body[n.start:n.end]), fieldKind{path: path, property: path[strings.LastIndexByte(path, '.')+1:], toolInput: tool})
		if err != nil {
			return err
		}
		if len(edits) > 0 {
			return &RejectError{Reason: "sensitive scalar requires a string field"}
		}
		return nil
	}
	if n.kind == '"' {
		return w.literal(n, fieldKind{path: path, property: path[strings.LastIndexByte(path, '.')+1:], toolInput: tool})
	}
	for _, p := range n.pairs {
		if !w.supportedOnly {
			if err := w.literal(p.key, fieldKind{path: path + "." + p.key.text, toolInput: tool, objectKey: true}); err != nil {
				return err
			}
		}
		if err := w.allStrings(p.value, path+"."+p.key.text, tool); err != nil {
			return err
		}
	}
	for i, v := range n.items {
		if err := w.allStrings(v, fmt.Sprintf("%s[%d]", path, i), tool); err != nil {
			return err
		}
	}
	return nil
}
func (w *jsonRewriter) schema(n *jsonNode, path string) error {
	for _, p := range n.pairs {
		next := path + "." + p.key.text
		switch p.key.text {
		case "description", "title":
			if err := w.literal(p.value, fieldKind{path: next}); err != nil {
				return err
			}
		case "properties", "$defs", "definitions", "patternProperties", "dependentSchemas":
			for _, property := range p.value.pairs {
				if err := w.schema(property.value, next+"."+property.key.text); err != nil {
					return err
				}
			}
		case "enum", "const", "default", "examples":
			if err := w.allStrings(p.value, next, false); err != nil {
				return err
			}
		default:
			// propertyNames constrains immutable object keys, so supported-only
			// traversal preserves that subtree along with unknown schema keywords.
			if w.supportedOnly && !strings.Contains(" items additionalProperties contains if then else not anyOf allOf oneOf prefixItems unevaluatedItems unevaluatedProperties ", " "+p.key.text+" ") {
				continue
			}
			if err := w.schema(p.value, next); err != nil {
				return err
			}
		}
	}
	for i, v := range n.items {
		if err := w.schema(v, fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	return nil
}
func (w *jsonRewriter) literal(n *jsonNode, field fieldKind) error {
	if n.kind != '"' {
		return nil
	}
	field.nodeStart = n.start
	raw := w.body[n.start+1 : n.end-1]
	var offsets []int
	field.rawSpan = func(a, b int) []byte {
		if offsets == nil {
			offsets = literalOffsets(raw)
		}
		return raw[offsets[a]:offsets[b]]
	}
	edits, err := w.f(n.text, field)
	if err != nil {
		return err
	}
	if len(edits) == 0 {
		return nil
	}
	if offsets == nil {
		offsets = literalOffsets(raw)
	}
	last := 0
	billingEnd := 0
	if !w.supportedOnly && field.system && strings.HasPrefix(n.text, "x-anthropic-billing-header:") {
		billingEnd = strings.IndexByte(n.text, '\n')
		if billingEnd < 0 {
			billingEnd = len(n.text)
		}
	}
	for _, e := range edits {
		if e.start < last || e.end < e.start || e.end >= len(offsets) || offsets[e.start] < 0 || offsets[e.end] < 0 {
			return errors.New("privacy: invalid text edit")
		}
		last = e.end
		if e.start < billingEnd {
			continue
		}
		a, b := offsets[e.start], offsets[e.end]
		encoded := e.raw
		if encoded == nil {
			encoded = encodeLike(e.value, raw[a:b])
		}
		if w.record != nil {
			if err := w.record(encoded, raw[a:b]); err != nil {
				return err
			}
		}
		w.edits = append(w.edits, rawEdit{n.start + 1 + a, n.start + 1 + b, encoded})
	}
	return nil
}

// Map decoded UTF-8 byte boundaries back to raw JSON, including surrogate
// pairs and lone surrogates. Unchanged escapes are copied from the original.
func literalOffsets(raw []byte) []int {
	offsets := []int{0}
	for i := 0; i < len(raw); {
		start := i
		r, n := utf8.DecodeRune(raw[i:])
		i += n
		if raw[start] == '\\' {
			i = start + 2
			if raw[start+1] == 'u' {
				x, _ := strconv.ParseUint(string(raw[start+2:start+6]), 16, 16)
				r = rune(x)
				i = start + 6
				if utf16.IsSurrogate(r) {
					if i+6 <= len(raw) && raw[i] == '\\' && raw[i+1] == 'u' {
						y, _ := strconv.ParseUint(string(raw[i+2:i+6]), 16, 16)
						decoded := utf16.DecodeRune(r, rune(y))
						if decoded != utf8.RuneError {
							r = decoded
							i += 6
						} else {
							r = utf8.RuneError
						}
					} else {
						r = utf8.RuneError
					}
				}
			} else {
				r = 'x'
			}
		}
		for j := 1; j < utf8.RuneLen(r); j++ {
			offsets = append(offsets, -1)
		}
		offsets = append(offsets, i)
	}
	return offsets
}
func encodeLike(s string, original []byte) []byte {
	escaped := bytes.Contains(original, []byte(`\u`))
	if !escaped {
		b, _ := json.Marshal(s)
		return b[1 : len(b)-1]
	}
	all := len(original) > 0
	for i := 0; i < len(original); {
		if i+6 <= len(original) && original[i] == '\\' && original[i+1] == 'u' {
			i += 6
		} else {
			all = false
			break
		}
	}
	var out strings.Builder
	for _, r := range s {
		if all || r > 127 {
			if r > 0xffff {
				a, b := utf16.EncodeRune(r)
				fmt.Fprintf(&out, `\u%04x\u%04x`, a, b)
			} else {
				fmt.Fprintf(&out, `\u%04x`, r)
			}
		} else {
			b, _ := json.Marshal(string(r))
			out.Write(b[1 : len(b)-1])
		}
	}
	return []byte(out.String())
}
