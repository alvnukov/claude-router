package privacy

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
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
	// Key 1 is the diagnosis key on which the old PRF left 10.8.0.0/16 as is.
	restoreOnlyNetwork(t, fixedKeyEngine(t, r, fixedKey(1)), true)
}

// With a random key a network value may be a fixed point of the permutation
// (10.8.0.0/16 one key in 256). It then leaves as is, and only so if the
// engine counted it; run with -count=1000.
func TestRestoreRunsOnlyNetworkDetectorsRandomKey(t *testing.T) {
	r, err := ParseRules([]byte(`{"entries":[{"kind":"person","forms":["Zorvex"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	restoreOnlyNetwork(t, testEngine(t, r), false)
}

func restoreOnlyNetwork(t *testing.T, e *Engine, strict bool) {
	t.Helper()
	body := requestBody("", "Zorvex 10.2.3.4 fd00:1234::7 10.8.0.0/16 02:42:ac:11:00:02\npassword=shh-SYNTHETIC-secret")
	masked, req := mustMask(t, e, body)
	var out struct{ System string }
	if err := json.Unmarshal(masked, &out); err != nil {
		t.Fatal(err)
	}
	for _, real := range []string{"Zorvex", "shh-SYNTHETIC-secret"} {
		if strings.Contains(out.System, real) {
			t.Fatalf("fixture value %q not masked", real)
		}
	}
	words := strings.Fields(out.System)
	for _, v := range []struct {
		real string
		kind Kind
	}{{"10.2.3.4", KindIPv4}, {"fd00:1234::7", KindIPv6}, {"10.8.0.0/16", KindCIDR4}, {"02:42:ac:11:00:02", KindMAC}} {
		if !slices.Contains(words, v.real) {
			continue
		}
		fixed := 0
		for _, n := range e.Counters().Fixed[v.kind] {
			fixed += n
		}
		if strict || fixed == 0 {
			t.Fatalf("fixture value %q not masked (fixed points counted: %d)", v.real, fixed)
		}
	}
	full := *e.detectors.regex.policy
	e.detectors.regex.policy = &detectionPolicy{rules: full.rules, ipv4: full.ipv4, ipv6: full.ipv6, mac: full.mac}
	back, err := e.UnmaskJSON(req, masked)
	if err != nil || !bytes.Equal(back, body) {
		t.Fatalf("network roundtrip changed (%v):\n%s\n%s", err, body, back)
	}
}
