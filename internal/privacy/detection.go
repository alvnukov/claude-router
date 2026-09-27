package privacy

// Detect reports configured matches without issuing aliases, loading a session
// or rewriting the caller's data. Only counts leave this call. The same semantic
// text walker and detectors as Mask cover supported content/tool/schema fields;
// this is a debugging aid, never a proof that arbitrary input is safe to send.
func (e *Engine) Detect(body []byte) (map[Kind]int, error) {
	if len(body) > TrafficInputLimit || ValidateObject(body) != nil {
		return nil, errTraffic
	}
	// This key is used only for ephemeral equality fingerprints and IP scope
	// selection. No generated value, fingerprint or key leaves this call.
	key := make([]byte, 32)
	ip, err := newIPMapper(key, e.rules)
	if err != nil {
		return nil, errTraffic
	}
	session := &sessionData{key: key}
	m := &mapper{e: e, req: &Request{ip: ip}, view: &sessionView{session: session}}
	// Discover plaintext secrets first so encoded occurrences can precede them.
	_, err = rewriteRequest(body, func(text string, f fieldKind) ([]textEdit, error) {
		if e.allowedPath(f.path) {
			return nil, nil
		}
		for _, span := range m.plainSpans(text, f) {
			if span.Kind == KindSecret {
				session.rememberSecret(span.Value)
			}
		}
		return nil, nil
	})
	if err != nil {
		return nil, errTraffic
	}
	counts := map[Kind]int{}
	var source func()
	if e.rules.Sources == "withhold" {
		source = func() { counts[KindSource]++ }
	}
	_, err = rewriteJSON(body, func(text string, f fieldKind) ([]textEdit, error) {
		if e.allowedPath(f.path) {
			return nil, nil
		}
		for _, at := range encodedTokenRE.FindAllStringIndex(text, -1) {
			if at[1]-at[0] > maxEncodedToken {
				return nil, errTraffic
			}
		}
		spans := append(m.plainSpans(text, f), m.encodedSpans(text)...)
		for _, span := range resolve(spans) {
			counts[span.Kind]++
		}
		return nil, nil
	}, source)
	if err != nil {
		return nil, errTraffic
	}
	return counts, nil
}
