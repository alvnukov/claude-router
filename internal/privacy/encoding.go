package privacy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Only keyed fingerprints survive the request. They recognize a known secret
// in supported encodings without retaining its plaintext for later unmasking.
const kindFingerprint Kind = "secret_fingerprint"
const maxEncodedToken = 64 << 10
const maxEncodingDepth = 3

var encodedTokenRE = regexp.MustCompile(`[A-Za-z0-9_%+/-]{8,}={0,2}`)

func (s *sessionData) fingerprint(value string) string {
	h := hmac.New(sha256.New, subkey(s.key, "secret-fingerprint"))
	_, _ = h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil))
}
func (s *sessionData) rememberSecret(value string) {
	if s.fingerprints == nil {
		s.fingerprints = make(map[string]bool)
	}
	digest := s.fingerprint(value)
	if !s.fingerprints[digest] {
		s.fingerprints[digest] = true
		s.pending = append(s.pending, mapRecord{Kind: kindFingerprint, Fingerprint: digest})
	}
}
func (s *sessionData) knowsSecret(value string) bool {
	if s.fingerprints[s.fingerprint(value)] {
		return true
	}
	// HTTP Basic credentials encode user:password as one value.
	if _, password, ok := strings.Cut(value, ":"); ok {
		return s.fingerprints[s.fingerprint(password)]
	}
	return false
}
func decodeRepresentations(token string) []string {
	var out []string
	add := func(b []byte, err error) {
		if err == nil && len(b) > 0 && utf8.Valid(b) && string(b) != token {
			out = append(out, string(b))
		}
	}
	if len(token) > maxEncodedToken {
		return nil
	}
	add(hex.DecodeString(token))
	for _, enc := range []*base64.Encoding{base64.StdEncoding.Strict(), base64.RawStdEncoding.Strict(), base64.URLEncoding.Strict(), base64.RawURLEncoding.Strict()} {
		add(enc.DecodeString(token))
	}
	if strings.Contains(token, "%") {
		value, err := url.QueryUnescape(token)
		add([]byte(value), err)
	}
	return out
}
func encodedSensitive(token string, depth int, sensitive func(string) bool) bool {
	if depth == 0 {
		return false
	}
	for _, decoded := range decodeRepresentations(token) {
		if sensitive(decoded) {
			return true
		}
		for _, nested := range encodedTokenRE.FindAllString(decoded, -1) {
			if encodedSensitive(nested, depth-1, sensitive) {
				return true
			}
		}
	}
	return false
}
func (m *mapper) encodedSpans(text string) []Span {
	var out []Span
	sensitive := func(decoded string) bool {
		if m.view.session.knowsSecret(decoded) {
			return true
		}
		for _, token := range encodedTokenRE.FindAllString(decoded, -1) {
			if m.view.session.knowsSecret(token) {
				return true
			}
		}
		return len(m.e.detectors.effectiveSpans(decoded, m.req.ip)) > 0
	}
	for _, at := range encodedTokenRE.FindAllStringIndex(text, -1) {
		token := text[at[0]:at[1]]
		if m.view.session.knowsSecret(token) || encodedSensitive(token, maxEncodingDepth, sensitive) {
			out = append(out, Span{at[0], at[1], KindSecret, token})
		}
	}
	return out
}
