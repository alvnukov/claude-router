package privacy

import (
	"net/netip"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

type dictPattern struct {
	re     *regexp.Regexp
	needle string
	kind   Kind
	stem   bool
}
type dictDetector struct{ patterns []dictPattern }
type regexDetector struct {
	filters map[string]bool
	domains []string
	policy  *detectionPolicy
}
type detectors struct {
	dict  dictDetector
	regex regexDetector
	rules *Rules
}

func newDetectors(r *Rules) *detectors {
	d := &detectors{rules: r, regex: regexDetector{domains: r.Domains, policy: r.policy, filters: r.Filters}}
	if !enabled(r.Filters, "dictionary") {
		return d
	}
	for _, e := range r.Entries {
		for _, form := range e.Forms {
			stem := strings.HasSuffix(form, "*")
			literal := strings.TrimSuffix(form, "*")
			pattern := regexp.QuoteMeta(literal)
			if stem {
				pattern += `[\pL\pM]*`
			}
			d.dict.patterns = append(d.dict.patterns, dictPattern{regexp.MustCompile("(?i)" + pattern), strings.Map(foldSimpleRune, literal), e.Kind, stem})
		}
	}
	return d
}

func wordRune(r rune, host bool) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || host && r == '-'
}
func wordBoundary(text string, start, end int, host bool) bool {
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(text[:start])
		if wordRune(r, host) {
			return false
		}
	}
	if end < len(text) {
		r, _ := utf8.DecodeRuneInString(text[end:])
		if wordRune(r, host) {
			return false
		}
	}
	return true
}

// Canonicalize each SimpleFold orbit so the prefilter cannot miss a regex
// match even when case folding changes the UTF-8 byte width (K and K).
func foldSimpleRune(r rune) rune {
	minimum := r
	for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
		if folded < minimum {
			minimum = folded
		}
	}
	return minimum
}
func (d dictDetector) Detect(text string) []Span {
	if len(d.patterns) == 0 {
		return nil
	}
	folded := strings.Map(foldSimpleRune, text)
	var out []Span
	for _, p := range d.patterns {
		if !strings.Contains(folded, p.needle) {
			continue
		}
		for _, v := range p.re.FindAllStringIndex(text, -1) {
			if wordBoundary(text, v[0], v[1], p.kind == KindHost) || pathBoundary(text, v[0], v[1]) {
				out = append(out, Span{v[0], v[1], p.kind, text[v[0]:v[1]]})
			}
		}
	}
	return out
}

func domainOf(host string, domains []string) string {
	host = strings.ToLower(host)
	best := ""
	for _, d := range domains {
		if (host == d || strings.HasSuffix(host, "."+d)) && len(d) > len(best) {
			best = d
		}
	}
	return best
}
func networkSpan(s string) (netip.Addr, Kind, bool) {
	if strings.Contains(s, "/") {
		p, e := netip.ParsePrefix(s)
		if e != nil {
			return netip.Addr{}, "", false
		}
		k := KindCIDR6
		if p.Addr().Is4() {
			k = KindCIDR4
		}
		return p.Addr(), k, true
	}
	a, e := netip.ParseAddr(s)
	if e != nil {
		return a, "", false
	}
	k := KindIPv6
	if a.Is4() {
		k = KindIPv4
	}
	return a, k, true
}
func (d regexDetector) Detect(text string) []Span {
	p := d.policy
	if p == nil {
		p = defaultPolicy
	}
	var out []Span
	if enabled(d.filters, "secret") {
		out = p.detectSecrets(text)
	}
	out = append(out, d.Network(text)...)
	for _, v := range p.host.FindAllStringIndex(text, -1) {
		s := text[v[0]:v[1]]
		if domainOf(s, d.domains) != "" {
			out = append(out, Span{v[0], v[1], KindHost, s})
		}
	}
	for _, v := range p.email.FindAllStringIndex(text, -1) {
		s := text[v[0]:v[1]]
		_, host, _ := strings.Cut(s, "@")
		lineStart := strings.LastIndexByte(text[:v[0]], '\n') + 1
		if strings.HasPrefix(strings.ToLower(text[lineStart:]), "message-id:") || v[0] > 0 && (text[v[0]-1] == ':' || text[v[0]-1] == '/') {
			continue
		}
		if domainOf(host, d.domains) != "" {
			out = append(out, Span{v[0], v[1], KindEmail, s})
		}
	}
	for _, v := range p.phone.FindAllStringIndex(text, -1) {
		s := text[v[0]:v[1]]
		digits := 0
		for _, c := range s {
			if c >= '0' && c <= '9' {
				digits++
			}
		}
		if digits >= 8 && digits <= 15 {
			out = append(out, Span{v[0], v[1], KindPhone, s})
		}
	}
	filtered := out[:0]
	for _, span := range out {
		if enabled(d.filters, string(span.Kind)) {
			filtered = append(filtered, span)
		}
	}
	return filtered
}

// Network returns only the address spans (IPv4, IPv6, CIDR, MAC) of Detect.
// Restoring a response needs no others, and the secret scans dominate the cost
// on large responses.
func (d regexDetector) Network(text string) []Span {
	p := d.policy
	if p == nil {
		p = defaultPolicy
	}
	var out []Span
	for _, re := range []*regexp.Regexp{p.ipv4, p.ipv6} {
		if re == p.ipv4 && !enabled(d.filters, "ipv4") || re == p.ipv6 && !enabled(d.filters, "ipv6") {
			continue
		}
		for _, v := range re.FindAllStringIndex(text, -1) {
			start, end := v[0], v[1]
			if re == p.ipv6 && start > 0 && (wordRune(rune(text[start-1]), true)) {
				if i := strings.IndexByte(text[start:end], ':'); i >= 0 {
					start += i + 1
				}
			}
			if !wordBoundary(text, start, end, false) || start > 0 && text[start-1] == '.' || end+1 < len(text) && text[end] == '.' && text[end+1] >= '0' && text[end+1] <= '9' {
				continue
			}
			value := text[start:end]
			if _, k, ok := networkSpan(value); ok {
				out = append(out, Span{start, end, k, value})
			}
		}
	}
	for _, v := range p.mac.FindAllStringIndex(text, -1) {
		s := text[v[0]:v[1]]
		if _, ok := parseMAC(s); ok && wordBoundary(text, v[0], v[1], false) {
			out = append(out, Span{v[0], v[1], KindMAC, s})
		}
	}
	filtered := out[:0]
	for _, span := range out {
		if enabled(d.filters, string(span.Kind)) {
			filtered = append(filtered, span)
		}
	}
	return filtered
}
func (d *detectors) allowed(s string) bool {
	for _, v := range d.rules.Allow {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
func (d *detectors) effectiveSpans(text string, m *ipMapper) []Span {
	spans := append(d.regex.Detect(text), d.dict.Detect(text)...)
	for _, p := range d.rules.patterns {
		if !enabled(d.rules.Filters, "patterns") {
			break
		}
		spans = append(spans, p.Detect(text)...)
	}
	spans = resolve(spans)
	var out []Span
	for _, s := range spans {
		if d.allowed(s.Value) {
			continue
		}
		if a, _, ok := networkSpan(s.Value); ok {
			if _, masked := m.maskAddr(a); !masked {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// A path component's underscore often separates a key filename from an
// organisation (for example id_ed25519_example). The corpus requires masking
// that suffix; ordinary identifiers still keep underscore word boundaries.
func pathBoundary(text string, start, end int) bool {
	if start == 0 || text[start-1] != '_' || !wordBoundary(text, start, start, false) && end < len(text) && wordRune(rune(text[end]), false) {
		return false
	}
	i := start - 1
	for i >= 0 && !unicode.IsSpace(rune(text[i])) && text[i] != '"' {
		if text[i] == '/' {
			return true
		}
		i--
	}
	return false
}
