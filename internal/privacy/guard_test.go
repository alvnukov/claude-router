package privacy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"
)

var placeholderRE = regexp.MustCompile(`<secret:[a-z0-9]+:[a-f0-9]{8}>`)

// A PRF that fails its known answers refuses the key: nothing is masked with
// it and nothing leaves, on Mask and on Detect alike.
func TestBrokenPRFRefused(t *testing.T) {
	saved := prfSelfTest
	prfSelfTest = func() error { return fmt.Errorf("broken PRF") }
	t.Cleanup(func() { prfSelfTest = saved })
	r, err := ParseRules([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	e := fixedKeyEngine(t, r, fixedKey(1))
	body := requestBody("prf", "gateway 10.2.3.4")
	if out, _, err := e.Mask(body); err == nil || out != nil {
		t.Errorf("mask with a broken PRF: %s, %v", out, err)
	}
	if _, err := e.Detect(body); err == nil {
		t.Error("detect with a broken PRF succeeded")
	}
}

// A value the network mapper does not handle must not leave equal to itself:
// 10.0.0.0/7 starts inside 10/8 but is wider than the block, so no block
// permutes it. It leaves as the tier-1 placeholder and counts as a skip.
func TestNetworkSkipStub(t *testing.T) {
	r, err := ParseRules([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	e := fixedKeyEngine(t, r, fixedKey(1))
	body := requestBody("", "net 10.0.0.0/7 end")
	masked, req := mustMask(t, e, body)
	var out struct{ System string }
	if err := json.Unmarshal(masked, &out); err != nil {
		t.Fatal(err)
	}
	if !placeholderRE.MatchString(out.System) || strings.Contains(out.System, "10.0.0.0/7") {
		t.Fatalf("unhandled value not a placeholder: %s", out.System)
	}
	if got := req.Stats().Unhandled; got != 1 {
		t.Fatalf("Unhandled = %d, want 1", got)
	}
	back, err := e.UnmaskJSON(req, masked)
	if err != nil || !bytes.Equal(back, body) {
		t.Fatalf("roundtrip changed (%v):\n%s\n%s", err, body, back)
	}
}

// On key 56 version 2 maps 10.8.0.0/16 to itself: a fixed point of a sound
// permutation, as likely as a guess (2^-8). It leaves as is and is counted.
func TestNetworkFixedPointCounted(t *testing.T) {
	r, err := ParseRules([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	e := fixedKeyEngine(t, r, fixedKey(56))
	body := requestBody("", "net 10.8.0.0/16 end")
	masked, req := mustMask(t, e, body)
	if !bytes.Contains(masked, []byte("net 10.8.0.0/16 end")) {
		t.Fatalf("fixed point not left as is: %s", masked)
	}
	c := e.Counters()
	if c.Permuted[KindCIDR4][8] != 1 || c.Fixed[KindCIDR4][8] != 1 {
		t.Fatalf("permuted %v, fixed %v; want one of each at k=8", c.Permuted, c.Fixed)
	}
	if req.Stats().Unhandled != 0 {
		t.Fatalf("fixed point counted as a skip")
	}
	// The same value again in the session is not a new observation.
	mustMask(t, e, body)
	if c := e.Counters(); c.Permuted[KindCIDR4][8] != 1 {
		t.Fatalf("repeated value counted again: %v", c.Permuted)
	}
}

// Each permutation class alarms on a permutation that leaves every value in
// place and stays silent on one that leaves them at the rate 2^-k. Each
// substitution class alarms on its first value left equal to itself. The
// alarm is one log line per engine, without values.
func TestFixedPointAlarm(t *testing.T) {
	for _, kind := range []Kind{KindIPv4, KindIPv6, KindCIDR4, KindCIDR6, KindMAC} {
		t.Run("permutation/"+string(kind), func(t *testing.T) {
			var sound, broken counters
			var lines []string
			broken.logf = func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }
			sound.logf = func(string, ...any) { t.Fatal("alarm on a sound permutation") }
			// k=8: E reaches 10 at 2560 values; a sound permutation leaves
			// every 256th in place, the broken one all of them.
			for i := range 2560 {
				d := fmt.Sprintf("%08x", i)
				sound.permutation(kind, 8, i%256 == 0, d)
				broken.permutation(kind, 8, true, d)
			}
			if sound.alarms[kind] || !broken.alarms[kind] || len(lines) != 1 {
				t.Fatalf("sound alarm %v, broken alarm %v, lines %q", sound.alarms[kind], broken.alarms[kind], lines)
			}
			if strings.Contains(lines[0], "0000") {
				t.Fatalf("alarm line carries a value: %q", lines[0])
			}
		})
	}
	for _, kind := range []Kind{KindHost, KindEmail, KindPhone, KindPerson, KindLogin, KindSecret} {
		t.Run("substitution/"+string(kind), func(t *testing.T) {
			var c counters
			var lines []string
			c.logf = func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }
			c.substitution(kind, false)
			if c.alarms[kind] {
				t.Fatal("alarm without a match")
			}
			c.substitution(kind, true)
			if !c.alarms[kind] || c.same[kind] != 1 || len(lines) != 1 {
				t.Fatalf("alarm %v, same %d, lines %q", c.alarms[kind], c.same[kind], lines)
			}
		})
	}
	t.Run("one line per engine", func(t *testing.T) {
		var c counters
		var lines []string
		c.logf = func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }
		c.substitution(KindHost, true)
		c.substitution(KindEmail, true)
		if len(lines) != 1 || !c.alarms[KindEmail] {
			t.Fatalf("lines %q, alarms %v", lines, c.alarms)
		}
	})
}

// Through the engine: twelve sessions on version 2, each masking all 256 /16
// networks of 10/8, stay silent. The old PRF left about 6.7% of them in place;
// its alarm is shown on the counter itself, since no session runs it now.
func TestFixedPointAlarmSessions(t *testing.T) {
	var nets []string
	for i := range 256 {
		nets = append(nets, fmt.Sprintf("10.%d.0.0/16", i))
	}
	text := strings.Join(nets, " ")
	r, err := ParseRules([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	next := uint64(2000)
	e, err := Open(t.TempDir(), r, Options{Home: "/home/testlogin", Hostname: "test-host.local", newKey: func() ([]byte, error) {
		next++
		return fixedKey(next), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	e.counts.logf = func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }
	for i := range 12 {
		mustMask(t, e, requestBody(fmt.Sprint("s", i), text))
	}
	c := e.Counters()
	if c.Permuted[KindCIDR4][8] != 12*256 {
		t.Fatalf("permuted %v", c.Permuted)
	}
	if c.Alarms[KindCIDR4] || len(lines) != 0 {
		t.Fatalf("fixed %d of %d (E=12, limit %.1f), alarm %v, lines %q", c.Fixed[KindCIDR4][8], 12*256, 12+3*math.Sqrt(12), c.Alarms, lines)
	}
}

// An explicit rule whose pseudonym is the original is refused at Open, so a
// substitution equal to its value is never the operator's choice and the
// counter needs no exception for it.
func TestSelfPseudonymRuleRejected(t *testing.T) {
	r, err := ParseRules([]byte(`{"entries":[{"kind":"person","forms":["Zorvex"],"pseudonym":"Zorvex"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.TempDir(), r, Options{}); err == nil || !strings.Contains(err.Error(), "pseudonym collision") {
		t.Fatalf("self pseudonym accepted: %v", err)
	}
}

// Verdict 3, item 3: a recognised value whose substitution comes back equal to
// itself leaves as a placeholder, for any class, and the request restores it.
// Rules refuse such a pseudonym at parse time; a session entry reaches past
// that check.
func TestSubstitutionFailureStub(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	const email = "olga.smirnova@vasilek.example"
	req := &Request{engine: e, secrets: make(map[string]string), spellings: make(map[string]string), stats: Stats{Masked: make(map[Kind]int)}}
	sd := &sessionData{key: fixedKey(1), entries: make(map[string]mapRecord)}
	m := &mapper{e: e, view: &sessionView{session: sd}, req: req}
	m.init()
	sd.entries[entityKey(KindEmail, email)] = mapRecord{Kind: KindEmail, Real: email, Pseudo: email}
	edits, err := m.maskText("mail "+email, fieldKind{})
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 1 || !strings.HasPrefix(edits[0].value, "<secret:email:") {
		t.Fatalf("value equal to itself left as is: %+v", edits)
	}
	if req.secrets[edits[0].value] != email {
		t.Fatalf("placeholder does not restore: %q", req.secrets[edits[0].value])
	}
}
