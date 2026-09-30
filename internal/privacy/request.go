package privacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"regexp"
	"strings"
	"sync"
)

// Stats contains counts only; it never contains original sensitive values.
type Stats struct {
	Scope            string
	Masked, Unmasked map[Kind]int
	Unexpected       int
	// Unhandled counts network values no block handled; each left as a
	// placeholder instead of equal to itself.
	Unhandled int
	// Version is the PRF version of the request's session.
	Version int
}

// Request owns the response dictionary and request-scoped secret values.
// Close releases its session lease after both JSON and streaming responses.
type Request struct {
	budget             *restoreBudget
	mu                 sync.Mutex
	engine             *Engine
	ip                 *ipMapper
	secrets, spellings map[string]string
	rawSpellings       map[string][]byte
	undo               trieNode
	issued             *sessionData
	stats              Stats
	release            func()
	closed             bool
}

func (r *Request) rememberRaw(masked, real []byte) error {
	var pseudo, source string
	_ = json.Unmarshal(append(append([]byte{'"'}, masked...), '"'), &pseudo)
	_ = json.Unmarshal(append(append([]byte{'"'}, real...), '"'), &source)
	// Re-masking a foreign alias must restore the real value, never its old
	// alias. Keep raw spelling only when it represents that resolved value.
	if resolved, ok := r.spellings[pseudo]; ok && source != resolved {
		real = encodeLike(resolved, real)
	}
	if r.rawSpellings == nil {
		r.rawSpellings = make(map[string][]byte)
	}
	if old, ok := r.rawSpellings[string(masked)]; ok && !bytes.Equal(old, real) {
		return errors.New("privacy: ambiguous original spelling")
	}
	r.rawSpellings[string(masked)] = bytes.Clone(real)
	return nil
}
func (r *Request) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stats
	s.Masked = maps.Clone(s.Masked)
	s.Unmasked = maps.Clone(s.Unmasked)
	return s
}
func (r *Request) Close() {
	r.mu.Lock()
	release := r.release
	r.release = nil
	r.closed = true
	r.secrets = nil
	r.spellings = nil
	r.rawSpellings = nil
	r.undo = trieNode{}
	r.issued = nil
	r.ip = nil
	r.mu.Unlock()
	if release != nil {
		release()
	}
}

var secretPlaceholderRE = regexp.MustCompile(`(?i)<secret:[A-Za-z0-9-]+:[a-f0-9]{8}>`)

func correctionError() error {
	return &RejectError{Reason: "invalid, changed or unavailable pseudonym; copy the exact value from the current context and retry"}
}

func (r *Request) unmaskText(text string, field fieldKind) ([]textEdit, error) {
	if r.closed {
		return nil, &RejectError{Reason: "request dictionary is closed"}
	}
	if r.engine.allowedPath(field.path) || r.engine.detectors.allowed(text) {
		return nil, nil
	}
	supportedOnly := r.engine.opt.supportedOnly
	if field.toolInput && !supportedOnly {
		if expected := r.engine.fieldKind(field); expected != "" {
			valid := false
			for _, match := range r.undo.matches(text) {
				if match.start == 0 && match.end == len(text) && match.value.pseudo == text && match.value.kind == expected && !match.value.foreign {
					valid = true
				}
			}
			if !valid {
				r.stats.Unexpected++
				return nil, correctionError()
			}
		}
		for _, token := range encodedTokenRE.FindAllString(text, -1) {
			if encodedSensitive(token, maxEncodingDepth, func(decoded string) bool {
				return len(r.undo.matches(decoded)) > 0 || strings.Contains(strings.ToLower(decoded), "<secret:")
			}) {
				return nil, correctionError()
			}
		}
	}
	matches := r.undo.matches(text)
	var spans []Span
	lookup := make(map[int]textMatch, len(matches))
	for _, m := range matches {
		spans = append(spans, Span{m.start, m.end, m.value.kind, text[m.start:m.end]})
		lookup[m.start] = m
	}
	spans = append(spans, r.engine.detectors.regex.Network(text)...)
	for _, p := range secretPlaceholderRE.FindAllStringIndex(text, -1) {
		spans = append(spans, Span{p[0], p[1], KindSecret, text[p[0]:p[1]]})
	}
	var edits []textEdit
	for _, s := range resolve(spans) {
		if r.engine.detectors.allowed(s.Value) {
			continue
		}
		var value string
		if m, ok := lookup[s.Start]; ok && m.end == s.End {
			if !m.value.secret && !wordBoundary(text, s.Start, s.End, m.value.kind == KindHost) && !pathBoundary(text, s.Start, s.End) {
				if field.toolInput && !supportedOnly {
					return nil, correctionError()
				}
				continue
			}
			if kind := r.engine.fieldKind(field); field.toolInput && !supportedOnly && kind != "" && kind != m.value.kind {
				return nil, correctionError()
			}
			if m.value.foreign || s.Value != m.value.pseudo {
				if supportedOnly {
					continue
				}
				r.stats.Unexpected++
				if field.toolInput && !supportedOnly {
					return nil, correctionError()
				}
				continue
			}
			if m.value.secret {
				value = m.value.real
			} else {
				value = m.value.real
			}
		} else if s.Kind == KindSecret {
			if supportedOnly {
				continue
			}
			r.stats.Unexpected++
			if field.toolInput {
				return nil, correctionError()
			}
			continue
		} else {
			if _, seen := r.spellings[s.Value]; supportedOnly && !seen {
				continue
			}
			// Only a whole token this session issued comes back; a value
			// merely shaped like a pseudonym passes byte for byte.
			if !wholeToken(text, s) || !r.issued.issuedNetwork(s.Kind, s.Value) {
				continue
			}
			value, _ = mapNetwork(r.ip, s.Value, true)
		}
		if original, ok := r.spellings[s.Value]; ok {
			value = original
		}
		if value != s.Value {
			var raw []byte
			if field.rawSpan != nil {
				raw = r.rawSpellings[string(field.rawSpan(s.Start, s.End))]
			}
			edits = append(edits, textEdit{s.Start, s.End, value, raw})
			r.stats.Unmasked[s.Kind]++
		}
	}
	if err := r.budget.charge(edits, field); err != nil {
		return nil, err
	}
	return edits, nil
}

// wholeToken reports whether the span stands alone in its class, so that
// 10.23.4.5 is not taken from inside 10.23.4.55 (decision 997b362). A
// neighbour extends the token when it is a character of the class, or a
// separator of the class with such a character beyond it; the dot that ends a
// sentence and the colon before a port leave an address whole.
func wholeToken(text string, s Span) bool {
	digit := func(b byte) bool { return b >= '0' && b <= '9' }
	char, beyond := digit, map[byte]func(byte) bool{'.': digit}
	switch s.Kind {
	case KindCIDR4:
		beyond['/'] = digit
	case KindIPv6, KindCIDR6:
		char, beyond = isHex, map[byte]func(byte) bool{':': func(b byte) bool { return isHex(b) || b == ':' }}
		if s.Kind == KindCIDR6 {
			beyond['/'] = digit
		}
	case KindMAC:
		char, beyond = isHex, map[byte]func(byte) bool{':': isHex, '-': isHex}
	}
	extends := func(i, step int) bool {
		if i < 0 || i >= len(text) {
			return false
		}
		if char(text[i]) {
			return true
		}
		next, sep := beyond[text[i]]
		j := i + step
		return sep && j >= 0 && j < len(text) && next(text[j])
	}
	return !extends(s.Start-1, -1) && !extends(s.End, 1)
}
