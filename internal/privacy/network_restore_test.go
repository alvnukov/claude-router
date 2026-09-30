package privacy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// networkInput holds one value of each network class; issuedNetworks masks it
// in session "net" and returns the pseudonyms in the same order.
var networkInput = []string{"10.2.3.4", "10.9.0.0/16", "02:42:ac:11:00:02", "fd00:1234::7"}

func issuedNetworks(t *testing.T, e *Engine) ([]string, *Request) {
	t.Helper()
	masked, req := mustMask(t, e, requestBody("net", strings.Join(networkInput, " ")))
	var out struct{ System string }
	if err := json.Unmarshal(masked, &out); err != nil {
		t.Fatal(err)
	}
	pseudo := strings.Fields(out.System)
	if len(pseudo) != len(networkInput) {
		t.Fatalf("masked %q", out.System)
	}
	for i := range pseudo {
		if pseudo[i] == networkInput[i] {
			t.Fatalf("%s is a fixed point on this key; pick another", networkInput[i])
		}
	}
	return pseudo, req
}

func unmaskSystem(t *testing.T, e *Engine, req *Request, text string) string {
	t.Helper()
	back, err := e.UnmaskJSON(req, requestBody("net", text))
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ System string }
	if err := json.Unmarshal(back, &out); err != nil {
		t.Fatal(err)
	}
	return out.System
}

// A network value comes back only when it is a pseudonym
// this session issued; an address merely shaped like one, even under the same
// network as the pseudonyms, passes byte for byte.
func TestRestoreOnlyIssuedNetworks(t *testing.T) {
	r, err := ParseRules([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	e := fixedKeyEngine(t, r, fixedKey(1))
	pseudo, req := issuedNetworks(t, e)
	if got := unmaskSystem(t, e, req, strings.Join(pseudo, " ")); got != strings.Join(networkInput, " ") {
		t.Fatalf("issued pseudonyms not restored: %q", got)
	}
	v4 := strings.Split(pseudo[0], ".")
	net16 := strings.Split(strings.TrimSuffix(pseudo[1], "/16"), ".")
	mac := strings.Split(pseudo[2], ":")
	foreign := []string{
		strings.Join(append(v4[:3:3], fmt.Sprint((atoi(v4[3])+1)%256)), "."),
		net16[0] + "." + net16[1] + ".1.2",
		net16[0] + "." + fmt.Sprint((atoi(net16[1])+1)%256) + ".0.0/16",
		strings.Join(append(mac[:5:5], "ff"), ":"),
		strings.TrimSuffix(pseudo[3], pseudo[3][len(pseudo[3])-1:]) + "0",
		"10.2.3.4", "10.9.0.0/16",
	}
	for _, v := range foreign {
		if got := unmaskSystem(t, e, req, "see "+v+" now"); got != "see "+v+" now" {
			t.Errorf("value outside the session set rewritten: %q -> %q", v, got)
		}
	}
	// The set outlives the request: a later request in the session restores.
	_, later := mustMask(t, e, requestBody("net", "Zorvex"))
	if got := unmaskSystem(t, e, later, pseudo[0]); got != networkInput[0] {
		t.Fatalf("pseudonym of an earlier request not restored: %q", got)
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// Only a whole token is restored. A pseudonym inside a longer
// value of its class stays as is; a pseudonym split across stream chunks comes
// back whole.
func TestRestoreWholeTokenOnly(t *testing.T) {
	r, err := ParseRules([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	e := fixedKeyEngine(t, r, fixedKey(1))
	pseudo, req := issuedNetworks(t, e)
	longer := []string{
		pseudo[0] + "5", "1" + pseudo[0], pseudo[0] + ".9",
		pseudo[1] + "0", "1" + pseudo[1], pseudo[1] + "/8",
		pseudo[2] + ":0a", "0" + pseudo[2], pseudo[2] + "-01",
		pseudo[3] + ":1", "a" + pseudo[3],
		// The colon after a host joins a longer IPv6.
		"kafka-1:" + pseudo[3],
	}
	for _, v := range longer {
		if got := unmaskSystem(t, e, req, "x "+v+" y"); got != "x "+v+" y" {
			t.Errorf("pseudonym restored inside %q: %q", v, got)
		}
	}
	// A MAC written in dotted groups: the dot joins a longer value too.
	masked, dotted := mustMask(t, e, requestBody("net", "aabb.ccdd.eeff"))
	var mac struct{ System string }
	if err := json.Unmarshal(masked, &mac); err != nil || mac.System == "aabb.ccdd.eeff" || strings.Count(mac.System, ".") != 2 {
		t.Fatalf("dotted MAC masked to %q, %v", mac.System, err)
	}
	for _, v := range []string{mac.System + ".0011", "0011." + mac.System} {
		if got := unmaskSystem(t, e, dotted, "x "+v+" y"); got != "x "+v+" y" {
			t.Errorf("dotted MAC pseudonym restored inside %q: %q", v, got)
		}
	}
	if got := unmaskSystem(t, e, dotted, "x "+mac.System+". y"); got != "x aabb.ccdd.eeff. y" {
		t.Errorf("dotted MAC before a sentence dot: %q", got)
	}
	// A separator without a character of the class beyond it ends the token:
	// the dot of a sentence, the colon before a port.
	for _, c := range []struct{ in, want string }{
		{"шлюз " + pseudo[0] + ".", "шлюз " + networkInput[0] + "."},
		{pseudo[0] + ":8080", networkInput[0] + ":8080"},
		{"сеть " + pseudo[1] + ".", "сеть " + networkInput[1] + "."},
		{pseudo[2] + ".", networkInput[2] + "."},
		{"[" + pseudo[3] + "]:443", "[" + networkInput[3] + "]:443"},
	} {
		if got := unmaskSystem(t, e, req, c.in); got != c.want {
			t.Errorf("whole token %q: got %q, want %q", c.in, got, c.want)
		}
	}
	for i, p := range pseudo {
		for _, c := range []struct{ in, want string }{
			{"x " + p + " y", "x " + networkInput[i] + " y"},
			{"x " + p + "5 y", "x " + p + "5 y"},
			// A dot at the end of a chunk waits for the next byte; the end
			// of the stream is a boundary.
			{"x " + p + ". y", "x " + networkInput[i] + ". y"},
			{"x " + p + ".", "x " + networkInput[i] + "."},
			{"x " + p, "x " + networkInput[i]},
		} {
			var sink bytes.Buffer
			w := e.NewStreamUnmasker(req, &sink)
			for _, ch := range c.in {
				if _, err := w.Write([]byte(streamDelta(0, "text_delta", string(ch)))); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := w.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if got := streamText(t, sink.String()); got != c.want {
				t.Errorf("stream %q: got %q, want %q", c.in, got, c.want)
			}
		}
	}
}

// After masking networks the session file holds no
// input address and no pseudonym in clear text, only fingerprints. Key 56
// leaves 10.8.0.0/16 and 10.9.0.0/16 in place, so fixed points are among the
// values and the file must not name them either.
func TestSessionFileHoldsNoAddresses(t *testing.T) {
	r, err := ParseRules([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	e := fixedKeyEngine(t, r, fixedKey(56))
	input := append([]string{"10.8.0.0/16"}, networkInput...)
	masked, _ := mustMask(t, e, requestBody("net", strings.Join(input, " ")))
	var out struct{ System string }
	if err := json.Unmarshal(masked, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.System, "10.8.0.0/16 ") {
		t.Fatalf("key 56 no longer a fixed point: %q", out.System)
	}
	file, err := os.ReadFile(filepath.Join(e.store.dir, "v2", "net.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range append(input, strings.Fields(out.System)...) {
		if bytes.Contains(file, []byte(v)) {
			t.Errorf("session file holds %q in clear text", v)
		}
	}
	// Fixed points are not issued: on this key 10.9.0.0/16 stays as well.
	issued := 0
	for i, v := range strings.Fields(out.System) {
		if v != input[i] {
			issued++
		}
	}
	lines := bytes.Split(bytes.TrimSpace(file), []byte("\n"))[1:]
	if issued != 3 || len(lines) != issued {
		t.Fatalf("%d records for %d issued pseudonyms:\n%s\n%s", len(lines), issued, file, out.System)
	}
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatal(err)
		}
		if _, ok := rec["fingerprint"]; !ok || rec["real"] != nil || rec["pseudo"] != nil {
			t.Errorf("record is not a bare fingerprint: %s", line)
		}
	}
}

// An IPv6 address before dots, at the end of a sentence or
// an ellipsis, is masked and comes back; the detector used to drop it whole.
func TestNetworkBeforeDots(t *testing.T) {
	r, err := ParseRules([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	e := fixedKeyEngine(t, r, fixedKey(1))
	for _, c := range []struct{ in, addr string }{
		{"шлюз fd00:1234::7.", "fd00:1234::7"},
		{"see fd00::7... later", "fd00::7"},
		{"mapped ::ffff:192.168.1.1.", "::ffff:192.168.1.1"},
		{"host 10.2.3.4.", "10.2.3.4"},
	} {
		body := requestBody("dots", c.in)
		masked, req := mustMask(t, e, body)
		if bytes.Contains(masked, []byte(c.addr)) {
			t.Errorf("%q left in clear: %s", c.addr, masked)
		}
		back, err := e.UnmaskJSON(req, masked)
		if err != nil || !bytes.Equal(back, body) {
			t.Errorf("%q not restored: %s, %v", c.in, back, err)
		}
	}
}

// An IPv4-mapped IPv6 address is masked exactly as the bare IPv4 inside it:
// the same block, the same pseudonym, a public one stays open. It used to
// leave in clear: no block holds the ::ffff: form.
func TestMappedIPv4InIPv6(t *testing.T) {
	r, err := ParseRules([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	e := fixedKeyEngine(t, r, fixedKey(1))
	bare, _ := mustMask(t, e, requestBody("mapped", "192.168.1.1"))
	var out struct{ System string }
	if err := json.Unmarshal(bare, &out); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ in, want string }{
		{"a ::ffff:192.168.1.1 b", "a ::ffff:" + out.System + " b"},
		{"a ::ffff:192.168.1.1.", "a ::ffff:" + out.System + "."},
		{"a ::ffff:8.8.8.8 b", "a ::ffff:8.8.8.8 b"},
		{"a ::ffff:10.8.0.0/112 b", ""},
	} {
		body := requestBody("mapped", c.in)
		masked, req := mustMask(t, e, body)
		var got struct{ System string }
		if err := json.Unmarshal(masked, &got); err != nil {
			t.Fatal(err)
		}
		if c.want != "" && got.System != c.want || c.want == "" && (got.System == c.in || !strings.HasPrefix(got.System, "a ::ffff:10.")) {
			t.Errorf("%q masked to %q, want %q", c.in, got.System, c.want)
		}
		back, err := e.UnmaskJSON(req, masked)
		if err != nil || !bytes.Equal(back, body) {
			t.Errorf("%q not restored: %s, %v", c.in, back, err)
		}
	}
}

// A value in the shape of its class that
// does not parse leaves as a placeholder and counts as a shape without parse,
// alarmed on the first; times, dates, versions and ports have no address
// shape and pass as before.
func TestShapeWithoutParse(t *testing.T) {
	r, err := ParseRules([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	e := fixedKeyEngine(t, r, fixedKey(1))
	plain := "at 12:30 and 10:14:02.482, v 2.14.3, build 10.0.19045.3693, 2026:10:03:18 on :8080"
	masked, _ := mustMask(t, e, requestBody("shape", plain))
	if !bytes.Equal(masked, requestBody("shape", plain)) || len(e.Counters().Unparsed) != 0 {
		t.Fatalf("text without an address shape changed: %s, %v", masked, e.Counters().Unparsed)
	}
	for _, c := range []struct {
		value string
		kind  Kind
	}{
		{"256.1.1.1", KindIPv4},
		{"fd00::7::1", KindIPv6},
		{"02:42-ac:11:00:02", KindMAC},
		{"10.0.0.0/33", KindCIDR4},
	} {
		body := requestBody("shape", "v "+c.value+" w")
		masked, req := mustMask(t, e, body)
		var got struct{ System string }
		if err := json.Unmarshal(masked, &got); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got.System, c.value) || !strings.Contains(got.System, "<secret:"+string(c.kind)+":") {
			t.Errorf("%q not a placeholder: %s", c.value, masked)
		}
		if back, err := e.UnmaskJSON(req, masked); err != nil || !bytes.Equal(back, body) {
			t.Errorf("%q not restored: %s, %v", c.value, back, err)
		}
		if n := e.Counters().Unparsed[c.kind]; n != 1 || !e.Counters().Alarms[c.kind] {
			t.Errorf("%q: shape without parse %d, alarm %v", c.value, n, e.Counters().Alarms[c.kind])
		}
		// Detect counts it as well and changes nothing.
		if _, err := e.Detect(body); err != nil {
			t.Fatal(err)
		}
		if n := e.Counters().Unparsed[c.kind]; n != 2 {
			t.Errorf("%q: detect did not count, %d", c.value, n)
		}
	}
}

// The way an address is
// written does not change what the router does with it. Every form of a
// generated private, public or loopback address, bare and before a dot, gets
// the outcome of its canonical form (masked, placeholder, open) and comes
// back byte for byte. An address with an IPv4 inside has the bare IPv4 as its
// canonical form.
func TestAddressFormsEquivalent(t *testing.T) {
	r, err := ParseRules([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	e := fixedKeyEngine(t, r, fixedKey(1))
	rng := rand.New(rand.NewPCG(17, 17))
	v4 := func(prefix string) netip.Addr {
		p := netip.MustParsePrefix(prefix)
		b := p.Addr().As4()
		for i := p.Bits() / 8; i < 4; i++ {
			b[i] = byte(rng.IntN(254) + 1)
		}
		return netip.AddrFrom4(b)
	}
	v6 := func(prefix string) netip.Addr {
		p := netip.MustParsePrefix(prefix)
		b := p.Addr().As16()
		for i := (p.Bits() + 7) / 8; i < 16; i++ {
			if rng.IntN(3) > 0 {
				b[i] = byte(rng.IntN(256))
			}
		}
		return netip.AddrFrom16(b)
	}
	var addrs []netip.Addr
	for range 6 {
		addrs = append(addrs,
			v4("10.0.0.0/8"), v4("172.16.0.0/16"), v4("192.168.0.0/16"), v4("169.254.0.0/16"),
			v4("8.0.0.0/8"), v4("203.0.113.0/24"), v4("127.0.0.0/8"),
			v6("fd00::/8"), v6("fe80::/64"), v6("2001:db8::/32"), v6("2a00::/16"))
	}
	addrs = append(addrs, netip.IPv6Loopback(), netip.IPv6Unspecified())
	outcome := func(in string) string {
		body := requestBody("forms", in)
		masked, req := mustMask(t, e, body)
		if back, err := e.UnmaskJSON(req, masked); err != nil || !bytes.Equal(back, body) {
			t.Errorf("%q not restored: %s, %v", in, back, err)
		}
		var got struct{ System string }
		if err := json.Unmarshal(masked, &got); err != nil {
			t.Fatal(err)
		}
		switch {
		case got.System == in:
			return "open"
		case strings.Contains(got.System, "<secret:"):
			return "placeholder"
		}
		return "masked"
	}
	table := map[string]map[string]bool{}
	for _, a := range addrs {
		canonical := a.String()
		for form, text := range addressForms(a) {
			for _, frame := range []string{"a %s b", "a %s."} {
				want, got := outcome(fmt.Sprintf(frame, canonical)), outcome(fmt.Sprintf(frame, text))
				if got != want {
					t.Errorf("%s %q: %s, canonical %q: %s", form, text, got, canonical, want)
				}
				class := "public"
				switch {
				case a.IsLoopback() || a.IsUnspecified():
					class = "loopback"
				case a.IsPrivate() || a.IsLinkLocalUnicast():
					class = "private"
				}
				if table[form] == nil {
					table[form] = map[string]bool{}
				}
				table[form][class+" "+got] = true
			}
		}
	}
	for form, seen := range table {
		t.Logf("%-22s %v", form, slices.Sorted(maps.Keys(seen)))
	}
}

// addressForms writes a in every form TestAddressFormsEquivalent
// checks. For an address with
// an IPv4 inside a is that IPv4.
func addressForms(a netip.Addr) map[string]string {
	forms := map[string]string{"canonical": a.String()}
	if a.Is4() {
		b := a.As4()
		hex := fmt.Sprintf("%x:%x", uint16(b[0])<<8|uint16(b[1]), uint16(b[2])<<8|uint16(b[3]))
		forms["::ffff:v4"] = "::ffff:" + a.String()
		forms["full ::ffff:v4"] = "0:0:0:0:0:ffff:" + a.String()
		forms["compat ::v4"] = "::" + a.String()
		forms["upper ::FFFF:v4"] = "::FFFF:" + a.String()
		forms["mapped hex"] = "::ffff:" + hex
		forms["mapped 8 groups"] = "0:0:0:0:0:ffff:" + hex
		forms["[::ffff:v4]:443"] = "[::ffff:" + a.String() + "]:443"
		return forms
	}
	b := a.As16()
	groups := make([]string, 8)
	for i := range groups {
		groups[i] = fmt.Sprintf("%x", uint16(b[2*i])<<8|uint16(b[2*i+1]))
	}
	forms["8 groups"] = strings.Join(groups, ":")
	forms["8 groups padded"] = a.StringExpanded()
	forms["upper"] = strings.ToUpper(a.String())
	forms["[v6]:443"] = "[" + a.String() + "]:443"
	forms["zone %eth0"] = a.String() + "%eth0"
	// The last 32 bits dotted, the longest zero run of the six groups before
	// them shortened.
	head := strings.Join(groups[:6], ":") + ":"
	for n := 6; n >= 2; n-- {
		run := strings.Repeat("0:", n)
		if i := strings.Index(":"+head, ":"+run); i >= 0 {
			head = head[:i] + ":" + head[i+len(run):]
			if i == 0 {
				head = ":" + head
			}
			break
		}
	}
	forms["mixed dotted"] = head + netip.AddrFrom4([4]byte(b[12:])).String()
	return forms
}
