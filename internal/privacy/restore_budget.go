package privacy

import (
	"bytes"
	"unicode/utf8"
)

// A restoration can amplify a tiny alias into a long credential. Charge its
// escaped representation BEFORE applyText/encodeLike/applyRaw allocate output.
// Shrinking edits do not provide credit, so this is a conservative upper bound.
type restoreBudget struct {
	remaining int
	stream    bool
}

func (b *restoreBudget) charge(edits []textEdit, field fieldKind) error {
	if b == nil {
		return nil
	}
	for _, edit := range edits {
		double := b.stream && field.toolInput
		var style []byte
		if field.rawSpan != nil {
			style = field.rawSpan(edit.start, edit.end)
		}
		escaped := bytes.Contains(style, []byte(`\u`))
		all := escaped
		for i := 0; all && i < len(style); i += 6 {
			if i+6 > len(style) || style[i] != '\\' || style[i+1] != 'u' {
				all = false
			}
		}
		size := encodedBound(edit.value, escaped, all, double)
		if edit.raw != nil {
			rawSize := len(edit.raw)
			if double {
				rawSize = encodedBound(string(edit.raw), false, false, false)
			}
			size = max(size, rawSize)
		}
		growth := max(0, size-(edit.end-edit.start))
		if growth > b.remaining {
			return errRestore
		}
		b.remaining -= growth
	}
	return nil
}
func encodedBound(value string, escaped, all, double bool) int {
	size := 0
	for _, r := range value {
		n := utf8.RuneLen(r)
		switch {
		case all || escaped && r > 127 || r == '<' || r == '>' || r == '&' || r == 0x2028 || r == 0x2029:
			n = 6
			if r > 0xffff {
				n = 12
			}
			if double {
				n += n / 6
			}
		case r == '"' || r == '\\':
			n = 2
			if double {
				n = 4
			}
		case r == '\n' || r == '\r' || r == '\t' || r == '\b' || r == '\f':
			n = 2
			if double {
				n = 3
			}
		case r < 32:
			n = 6
			if double {
				n = 7
			}
		}
		size += n
	}
	return size
}

type trafficBuffer struct{ bytes.Buffer }

func (b *trafficBuffer) Write(p []byte) (int, error) {
	if len(p) > TrafficOutputLimit-b.Len() {
		return 0, errRestore
	}
	return b.Buffer.Write(p)
}
