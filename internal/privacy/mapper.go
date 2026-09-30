package privacy

import (
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/netip"
	"strings"
	"unicode/utf8"
)

type mapper struct {
	e     *Engine
	view  *sessionView
	req   *Request
	names *pseudonyms
	known trieNode
}

func (m *mapper) init() {
	m.names = &pseudonyms{words: m.e.words, reserved: make(map[string]bool), endings: m.e.rules.policy.rules.Endings, candidate: m.e.opt.candidate}
	for _, v := range m.e.rules.Allow {
		m.names.reserved[strings.ToLower(v)] = true
	}
	for _, e := range m.e.rules.Entries {
		for _, form := range e.Forms {
			m.names.reserved[strings.ToLower(strings.TrimSuffix(form, "*"))] = true
		}
		if e.Pseudonym != "" {
			m.names.reserved[strings.ToLower(e.Pseudonym)] = true
		}
	}
	for _, s := range m.view.all {
		for _, r := range s.entries {
			m.names.reserved[strings.ToLower(r.Real)] = true
			m.names.reserved[strings.ToLower(r.Pseudo)] = true
			m.known.add(r.Pseudo, matchValue{real: r.Real, kind: r.Kind, foreign: s != m.view.session})
		}
	}
	for _, r := range m.view.session.entries {
		m.known.add(r.Pseudo, matchValue{real: r.Real, kind: r.Kind})
	}
}
func (m *mapper) mapValue(real string, kind Kind) (string, error) {
	key := strings.ToLower(real)
	if r, ok := m.view.session.entries[entityKey(kind, real)]; ok {
		return r.Pseudo, nil
	}
	var pseudo string
	var err error
	var explicit bool
	switch kind {
	case KindHost:
		domain := domainOf(real, m.e.rules.Domains)
		if domain != "" && key != domain {
			suffix, err := m.mapValue(domain, KindHost)
			if err != nil {
				return "", err
			}
			prefix := real[:len(real)-len(domain)]
			// Explicit host entries in interior labels are private as well.
			parts := strings.Split(prefix, ".")
			for i, p := range parts {
				if spans := m.e.detectors.dict.Detect(p); len(spans) == 1 && spans[0].Start == 0 && spans[0].End == len(p) {
					parts[i], err = m.mapValue(p, KindHost)
					if err != nil {
						return "", err
					}
				}
			}
			pseudo = strings.Join(parts, ".") + suffix
		} else if domain != "" {
			tld := domain[strings.LastIndexByte(domain, '.')+1:]
			word, err := m.names.issue("fictitious-fantastic", subkey(m.view.session.key, "name:"+domain))
			if err != nil {
				return "", err
			}
			pseudo = word + "." + tld
		}
	case KindEmail:
		local, host, ok := strings.Cut(real, "@")
		if !ok {
			return "", errors.New("privacy: invalid email span")
		}
		hostPseudo, err := m.mapValue(host, KindHost)
		if err != nil {
			return "", err
		}
		localPseudo, err := m.names.issue(local, subkey(m.view.session.key, "name:email"))
		if err != nil {
			return "", err
		}
		pseudo = localPseudo + "@" + hostPseudo
	case KindPhone:
		for i := 0; i < 4096; i++ {
			candidate := phonePseudo(real, subkey(m.view.session.key, "phone"), i)
			if !m.names.collision(candidate) {
				pseudo = candidate
				break
			}
		}
		if pseudo == "" {
			return "", errors.New("privacy: phone pseudonym space exhausted")
		}
	}
	if pseudo == "" {
		for _, e := range m.e.rules.Entries {
			if e.Kind != kind {
				continue
			}
			for _, form := range e.Forms {
				if n, ok := formPrefix(real, form); ok {
					if e.Pseudonym != "" {
						pseudo = e.Pseudonym + real[n:]
						explicit = true
					} else if strings.HasSuffix(form, "*") && len(real) > n {
						basePseudo, err := m.mapValue(real[:n], kind)
						if err != nil {
							return "", err
						}
						pseudo = basePseudo + real[n:]
						explicit = false
					}
				}
			}
		}
		if pseudo == "" {
			pseudo, err = m.names.issue(real, subkey(m.view.session.key, "name:"+string(kind)))
			if err != nil {
				return "", err
			}
		}
	}
	for _, sd := range m.view.all {
		for _, issued := range sd.entries {
			if strings.EqualFold(issued.Pseudo, pseudo) {
				if explicit && issued.Kind == kind && issued.Real == real && issued.Pseudo == pseudo && explicitPseudonym(m.e.rules, kind, real) == pseudo {
					continue
				}
				return "", &RejectError{Reason: "pseudonym is already assigned"}
			}
		}
	}
	m.names.reserved[strings.ToLower(pseudo)] = true
	record := mapRecord{Kind: kind, Real: real, Pseudo: pseudo, At: m.e.opt.Now()}
	m.view.session.add(record)
	m.known.add(pseudo, matchValue{real: real, kind: kind})
	if explicit {
		return pseudo, nil
	}
	return caseLike(pseudo, real), nil
}
func (m *mapper) plainSpans(text string, field fieldKind) []Span {
	spans := m.e.detectors.effectiveSpans(text, m.req.ip)
	if kind := m.e.fieldKind(field); kind != "" && text != "" && (kind != KindSecret || !placeholderValue(text)) && !m.e.detectors.allowed(text) {
		spans = []Span{{0, len(text), kind, text}}
	}
	return spans
}
func (m *mapper) maskText(text string, field fieldKind) ([]textEdit, error) {
	if m.e.allowedPath(field.path) {
		return nil, nil
	}
	for _, at := range encodedTokenRE.FindAllStringIndex(text, -1) {
		if at[1]-at[0] > maxEncodedToken {
			return nil, &RejectError{Reason: "encoded token exceeds analysis limit"}
		}
	}
	spans := m.plainSpans(text, field)
	spans = append(spans, m.encodedSpans(text)...)
	known := m.known.matches(text)
	lookup := make(map[int]textMatch, len(known))
	for _, match := range known {
		spans = append(spans, Span{match.start, match.end, match.value.kind, text[match.start:match.end]})
		lookup[match.start] = match
	}
	spans = resolve(spans)
	var edits []textEdit
	for _, s := range spans {
		value, network := "", false
		original := s.Value
		var err error
		if k, ok := lookup[s.Start]; ok && k.end == s.End {
			if !k.value.foreign {
				continue
			}
			original = k.value.real
			value, err = m.mapValue(original, s.Kind)
		} else {
			switch {
			case networkKind(s.Kind):
				network = true
				value, err = m.mapNetwork(s)
			case s.Kind == KindSecret:
				m.view.session.rememberSecret(s.Value)
				value, err = m.placeholder(secretFamily(s.Value), s.Value)
			default:
				value, err = m.mapValue(s.Value, s.Kind)
			}
		}
		if err != nil {
			return nil, err
		}
		if !network {
			m.e.counts.substitution(s.Kind, value == original)
			// A recognised value never leaves as itself: a substitution
			// that failed leaves as a placeholder instead.
			if value == original {
				if value, err = m.placeholder(string(s.Kind), original); err != nil {
					return nil, err
				}
			}
		}
		if value != s.Value && m.e.detectors.allowed(value) {
			return nil, &RejectError{Reason: "pseudonym collides with an allowed value"}
		}
		m.req.spellings[value] = original
		if value != s.Value {
			edits = append(edits, textEdit{s.Start, s.End, value, nil})
			m.req.stats.Masked[s.Kind]++
		}
	}
	return edits, nil
}

// placeholder is the tier-1 stand-in <secret:family:hmac8> for a value that
// leaves in no form; the request restores it from req.secrets.
func (m *mapper) placeholder(family, value string) (string, error) {
	h := hmac.New(sha256.New, subkey(m.view.session.key, "secret"))
	h.Write([]byte(value))
	out := "<secret:" + family + ":" + hex.EncodeToString(h.Sum(nil)[:4]) + ">"
	if old, ok := m.req.secrets[out]; ok && old != value {
		return "", errors.New("privacy: secret placeholder collision")
	}
	m.req.secrets[out] = value
	return out, nil
}

// ambiguousAddress reports a value that does not parse only for leading zeros
// in its dotted groups: 010.001.002.003 reads as 10.1.2.3 and, the inet_aton
// way, as 8.1.2.3. It has no canonical form to be masked as.
func ambiguousAddress(s string) bool {
	end := len(s)
	if i := strings.IndexAny(s, "/%"); i >= 0 {
		end = i
	}
	start := strings.LastIndexByte(s[:end], ':') + 1
	if !ipv4Shape(s[start:end]) {
		return false
	}
	groups := strings.Split(s[start:end], ".")
	zeros := false
	for i, g := range groups {
		if t := strings.TrimLeft(g, "0"); t != g && g != "0" {
			zeros = true
			groups[i] = cmp.Or(t, "0")
		}
	}
	_, _, ok := networkSpan(s[:start] + strings.Join(groups, ".") + s[end:])
	return zeros && ok
}

// mapNetwork masks an address, a network or a MAC. A value under the
// threshold leaves as a placeholder, and so does one no block handles
// (counted as unhandled) rather than leave equal to itself, and one in the
// shape of its class that does not parse (counted as unparsed).
func (m *mapper) mapNetwork(s Span) (string, error) {
	if unparsedAddress(s.Value) {
		m.e.counts.unparsedShape(s.Kind, ambiguousAddress(s.Value))
		return m.placeholder(string(s.Kind), s.Value)
	}
	k, under, ok := m.req.ip.freeBits(s.Value)
	value := s.Value
	if ok && !under {
		value, ok = mapNetwork(m.req.ip, s.Value, false)
	}
	if !ok {
		m.req.stats.Unhandled++
	}
	if !ok || under {
		return m.placeholder(string(s.Kind), s.Value)
	}
	m.e.counts.permutation(s.Kind, k, value == s.Value, m.view.session.fingerprint(string(s.Kind)+":"+s.Value))
	// A fixed point stays out of the set: it passes back as is, which is
	// its original.
	if value != s.Value {
		m.view.session.issueNetwork(s.Kind, value)
	}
	return value, nil
}

func mapNetwork(m *ipMapper, value string, inverse bool) (string, bool) {
	ok := false
	if mac, isMAC := parseMAC(value); isMAC {
		if inverse {
			mac = m.unmaskMAC(mac)
		} else {
			mac = m.maskMAC(mac)
		}
		return formatMACLike(value, mac), true
	}
	if p, err := netip.ParsePrefix(value); err == nil {
		if inverse {
			p, ok = m.unmaskPrefix(p)
		} else {
			p, ok = m.maskPrefix(p)
		}
		return p.String(), ok
	}
	if a, err := netip.ParseAddr(value); err == nil {
		if inverse {
			a, ok = m.unmaskAddr(a)
		} else {
			a, ok = m.maskAddr(a)
		}
		return a.String(), ok
	}
	return value, false
}

// Unicode case folding preserves rune count, not UTF-8 byte length (K / K).
func formPrefix(real, form string) (int, bool) {
	base := strings.TrimSuffix(form, "*")
	n := 0
	for range utf8.RuneCountInString(base) {
		if n >= len(real) {
			return 0, false
		}
		_, size := utf8.DecodeRuneInString(real[n:])
		n += size
	}
	return n, strings.EqualFold(real[:n], base) && (n == len(real) || strings.HasSuffix(form, "*"))
}

// explicitPseudonym is the effective configured mapping, not just any
// matching entry: later entries and stem expansions can replace it.
func explicitPseudonym(rules *Rules, kind Kind, real string) string {
	if rules == nil || kind == KindEmail || kind == KindPhone || (kind == KindHost && domainOf(real, rules.Domains) != "") {
		return ""
	}
	var pseudo string
	for _, entry := range rules.Entries {
		if entry.Kind != kind {
			continue
		}
		for _, form := range entry.Forms {
			if n, ok := formPrefix(real, form); ok {
				if entry.Pseudonym != "" {
					pseudo = entry.Pseudonym + real[n:]
				} else if strings.HasSuffix(form, "*") && len(real) > n {
					pseudo = ""
				}
			}
		}
	}
	return pseudo
}
func entryMatches(entry Entry, real string) bool {
	for _, form := range entry.Forms {
		if _, ok := formPrefix(real, form); ok {
			return true
		}
	}
	return false
}
