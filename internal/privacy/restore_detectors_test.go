package privacy

import (
	"bytes"
	"testing"
)

// Restoring a response keeps only network spans from the regex detectors, so
// the secret, host, email and phone scans are pure cost on large responses.
// A policy without those regexes proves restore never runs them.
func TestRestoreRunsOnlyNetworkDetectors(t *testing.T) {
	r, err := ParseRules([]byte(`{"entries":[{"kind":"person","forms":["Zorvex"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, r)
	body := requestBody("", "Zorvex 10.2.3.4 fd00:1234::7 10.8.0.0/16 02:42:ac:11:00:02\npassword=shh-SYNTHETIC-secret")
	masked, req := mustMask(t, e, body)
	for _, real := range []string{"10.2.3.4", "fd00:1234::7", "10.8.0.0/16", "02:42:ac:11:00:02", "shh-SYNTHETIC-secret"} {
		if bytes.Contains(masked, []byte(real)) {
			t.Fatalf("fixture value %q not masked", real)
		}
	}
	full := *e.detectors.regex.policy
	e.detectors.regex.policy = &detectionPolicy{rules: full.rules, ipv4: full.ipv4, ipv6: full.ipv6, mac: full.mac}
	back, err := e.UnmaskJSON(req, masked)
	if err != nil || !bytes.Equal(back, body) {
		t.Fatalf("network roundtrip changed (%v):\n%s\n%s", err, body, back)
	}
}
