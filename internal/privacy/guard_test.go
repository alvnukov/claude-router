package privacy

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var placeholderRE = regexp.MustCompile(`<secret:[a-z0-9]+:[a-f0-9]{8}>`)

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

// Through the engine: sessions on the old PRF leave /16 networks of 10/8 in
// place about 6.7% of the time and raise the alarm; the same traffic on
// version 2 does not.
func TestFixedPointAlarmSessions(t *testing.T) {
	var nets []string
	for i := range 256 {
		nets = append(nets, fmt.Sprintf("10.%d.0.0/16", i))
	}
	text := strings.Join(nets, " ")
	for _, version := range []int{prfV1, prfV2} {
		t.Run(fmt.Sprint("version ", version), func(t *testing.T) {
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
				id := fmt.Sprint("s", i)
				if version == prfV1 {
					writeV1Session(t, e, id, fixedKey(3000+uint64(i)))
				}
				mustMask(t, e, requestBody(id, text))
			}
			c := e.Counters()
			if c.Permuted[KindCIDR4][8] != 12*256 {
				t.Fatalf("permuted %v", c.Permuted)
			}
			f := float64(c.Fixed[KindCIDR4][8])
			want := version == prfV1
			if c.Alarms[KindCIDR4] != want || (len(lines) == 1) != want {
				t.Fatalf("fixed %v of %d (E=%.1f, limit %.1f), alarm %v, lines %q", f, 12*256, 12.0, 12+3*math.Sqrt(12), c.Alarms, lines)
			}
		})
	}
}

func writeV1Session(t *testing.T, e *Engine, id string, key []byte) {
	t.Helper()
	line, err := json.Marshal(map[string]string{"kind": "key", "key": hex.EncodeToString(key)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.store.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.store.dir, id+".jsonl"), append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
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
