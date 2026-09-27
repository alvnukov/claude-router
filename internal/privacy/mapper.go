package privacy

import (
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
		value := ""
		original := s.Value
		var err error
		if k, ok := lookup[s.Start]; ok && k.end == s.End {
			if !k.value.foreign {
				continue
			}
			original = k.value.real
			value, err = m.mapValue(original, s.Kind)
		} else {
			switch s.Kind {
			case KindIPv4, KindIPv6, KindCIDR4, KindCIDR6, KindMAC:
				value = mapNetwork(m.req.ip, s.Value, false)
			case KindSecret:
				m.view.session.rememberSecret(s.Value)
				h := hmac.New(sha256.New, subkey(m.view.session.key, "secret"))
				h.Write([]byte(s.Value))
				value = "<secret:" + secretFamily(s.Value) + ":" + hex.EncodeToString(h.Sum(nil)[:4]) + ">"
				if old, ok := m.req.secrets[value]; ok && old != s.Value {
					return nil, errors.New("privacy: secret placeholder collision")
				}
				m.req.secrets[value] = s.Value
			default:
				value, err = m.mapValue(s.Value, s.Kind)
			}
		}
		if err != nil {
			return nil, err
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
func mapNetwork(m *ipMapper, value string, inverse bool) string {
	if mac, ok := parseMAC(value); ok {
		if inverse {
			mac = m.unmaskMAC(mac)
		} else {
			mac = m.maskMAC(mac)
		}
		return formatMACLike(value, mac)
	}
	if p, err := netip.ParsePrefix(value); err == nil {
		if inverse {
			p, _ = m.unmaskPrefix(p)
		} else {
			p, _ = m.maskPrefix(p)
		}
		return p.String()
	}
	if a, err := netip.ParseAddr(value); err == nil {
		if inverse {
			a, _ = m.unmaskAddr(a)
		} else {
			a, _ = m.maskAddr(a)
		}
		return a.String()
	}
	return value
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
