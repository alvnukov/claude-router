package privacy

import (
	"bytes"
	"errors"
	"math/bits"
	"math/rand/v2"
	"net/netip"
	"strings"
	"testing"
)

var testIPKey = bytes.Repeat([]byte{7}, 32)

func testMapper(t *testing.T, rules string) *ipMapper {
	t.Helper()
	r, err := ParseRules([]byte(rules))
	if err != nil {
		t.Fatal(err)
	}
	m, err := newIPMapper(testIPKey, r, prfV2)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// randIn returns a random address inside p.
func randIn(rng *rand.Rand, p netip.Prefix) netip.Addr {
	b := p.Addr().AsSlice()
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
	return withPrefixBits(b, p)
}

// withPrefixBits overwrites the first p.Bits() bits of b with p's.
func withPrefixBits(b []byte, p netip.Prefix) netip.Addr {
	head := p.Addr().AsSlice()
	n := p.Bits()
	for i := 0; n > 0; i++ {
		if n >= 8 {
			b[i] = head[i]
			n -= 8
			continue
		}
		mask := byte(0xff << (8 - n))
		b[i] = head[i]&mask | b[i]&^mask
		n = 0
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

// commonBits is the length of the common prefix of a and b.
func commonBits(a, b netip.Addr) int {
	x, y := a.AsSlice(), b.AsSlice()
	for i := range x {
		if d := x[i] ^ y[i]; d != 0 {
			return i*8 + bits.LeadingZeros8(d)
		}
	}
	return len(x) * 8
}

// TestPrefixPreserving: inside every masked class, two addresses keep a
// common prefix of exactly the same length, the image stays in the class and
// the inverse undoes it on any address of the class.
func TestPrefixPreserving(t *testing.T) {
	m := testMapper(t, `{}`)
	rng := rand.New(rand.NewChaCha8([32]byte{2}))
	classes := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "fd00::/8", "fe80::/10"}
	pairs := 0
	for _, c := range classes {
		class := netip.MustParsePrefix(c)
		depth := class.Addr().BitLen()
		if c == "fe80::/10" {
			depth = 64 // below the upper half the interface ID is not a prefix
		}
		for range 10000 / len(classes) {
			a := randIn(rng, class)
			// b shares exactly k leading bits with a.
			k := class.Bits() + rng.IntN(a.BitLen()-class.Bits())
			b := randIn(rng, netip.PrefixFrom(a, k+1))
			bs := b.AsSlice()
			bs[k/8] ^= 0x80 >> (k % 8)
			b, _ = netip.AddrFromSlice(bs)
			pairs++
			ma, oka := m.maskAddr(a)
			mb, okb := m.maskAddr(b)
			if !oka || !okb {
				t.Fatalf("%s or %s not masked", a, b)
			}
			if !class.Contains(ma) || !class.Contains(mb) {
				t.Fatalf("%s → %s, %s → %s: image left %s", a, ma, b, mb, class)
			}
			want := min(commonBits(a, b), depth)
			if got := min(commonBits(ma, mb), depth); got != want {
				t.Fatalf("%s, %s share %d bits; images %s, %s share %d", a, b, want, ma, mb, got)
			}
			if back, _ := m.unmaskAddr(ma); back != a {
				t.Fatalf("unmask(mask(%s)) = %s", a, back)
			}
			y := randIn(rng, class)
			x, ok := m.unmaskAddr(y)
			if again, _ := m.maskAddr(x); !ok || again != y {
				t.Fatalf("mask(unmask(%s)) = %s", y, again)
			}
		}
	}
	if pairs < 9996 {
		t.Fatalf("only %d pairs", pairs)
	}

	t.Run("cidr", func(t *testing.T) {
		for _, s := range []string{"10.113.0.0/16", "172.20.0.0/14", "169.254.0.0/16", "fd12:3456:789a::/48", "fe80::/64", "10.0.0.0/8"} {
			p := netip.MustParsePrefix(s)
			mp, ok := m.maskPrefix(p)
			if !ok || mp != mp.Masked() || mp.Bits() != p.Bits() {
				t.Errorf("network %s → %s: not a network of the same length", p, mp)
			}
			if back, _ := m.unmaskPrefix(mp); back != p {
				t.Errorf("unmask(mask(%s)) = %s", p, back)
			}
		}
		for _, s := range []string{"169.254.12.7/16", "10.113.8.1/24", "fd12:3456:789a:8::1/64", "fe80::200:5eff:fe00:5301/64", "10.250.0.2/32", "fd12:3456:789a::2/128"} {
			p := netip.MustParsePrefix(s)
			mp, _ := m.maskPrefix(p)
			if ma, _ := m.maskAddr(p.Addr()); mp.Addr() != ma || mp.Bits() != p.Bits() {
				t.Errorf("interface %s → %s, its address → %s", p, mp, ma)
			}
			if back, _ := m.unmaskPrefix(mp); back != p {
				t.Errorf("unmask(mask(%s)) = %s", p, back)
			}
		}
		// The swap: the address of a block whose image is the image block's
		// network address takes the image of the block's own network address.
		swapped := false
		for i := 0; i < 4096 && !swapped; i++ {
			z := netip.AddrFrom4([4]byte{10, byte(i >> 4), byte(i << 4), 0})
			block := netip.PrefixFrom(z, 28)
			hz, _ := m.maskAddr(z)
			zImage := netip.PrefixFrom(hz, 28).Masked()
			seen := map[netip.Prefix]bool{}
			for a := z; block.Contains(a); a = a.Next() {
				p := netip.PrefixFrom(a, 28)
				mp, ok := m.maskPrefix(p)
				if !ok || mp.Masked() != zImage || seen[mp] {
					t.Fatalf("%s → %s: outside %s or taken twice", p, mp, zImage)
				}
				seen[mp] = true
				if back, _ := m.unmaskPrefix(mp); back != p {
					t.Fatalf("unmask(mask(%s)) = %s", p, back)
				}
				if ha, _ := m.maskAddr(a); a != z && ha == zImage.Addr() {
					if mp.Addr() != hz {
						t.Fatalf("swap case %s → %s, want %s", p, mp, hz)
					}
					swapped = true
				}
			}
			if mz, _ := m.maskPrefix(block); mz != zImage {
				t.Fatalf("network %s → %s, want %s", block, mz, zImage)
			}
		}
		if !swapped {
			t.Fatal("no swap case found")
		}
	})
}

// TestNetworkBlocks: declared networks map onto their blocks in the pseudonym
// range, one to one; other public addresses stay as they are.
func TestNetworkBlocks(t *testing.T) {
	m := testMapper(t, `{"networks": ["198.51.100.0/24", "203.0.113.0/24", "2001:db8:4a1::/48"]}`)
	for _, c := range []struct{ real, pseudo string }{
		{"198.51.100.0/24", "100.64.0.0/24"},
		{"203.0.113.0/24", "100.64.1.0/24"},
	} {
		real, pseudo := netip.MustParsePrefix(c.real), netip.MustParsePrefix(c.pseudo)
		seen := map[netip.Addr]bool{}
		for a := real.Addr(); real.Contains(a); a = a.Next() {
			ma, ok := m.maskAddr(a)
			if !ok || !pseudo.Contains(ma) || seen[ma] {
				t.Fatalf("%s → %s: outside %s or taken twice", a, ma, pseudo)
			}
			seen[ma] = true
			if back, ok := m.unmaskAddr(ma); !ok || back != a {
				t.Fatalf("unmask(mask(%s)) = %s", a, back)
			}
		}
		if mp, _ := m.maskPrefix(real); mp != pseudo {
			t.Errorf("network %s → %s, want %s", real, mp, pseudo)
		}
	}
	rng := rand.New(rand.NewChaCha8([32]byte{3}))
	net6, block6 := netip.MustParsePrefix("2001:db8:4a1::/48"), netip.MustParsePrefix("3fff::/48")
	for range 1000 {
		a := randIn(rng, net6)
		ma, _ := m.maskAddr(a)
		if !block6.Contains(ma) {
			t.Fatalf("%s → %s outside %s", a, ma, block6)
		}
		if back, _ := m.unmaskAddr(ma); back != a {
			t.Fatalf("unmask(mask(%s)) = %s", a, back)
		}
	}
	sub := netip.MustParsePrefix("198.51.100.64/28")
	msub, _ := m.maskPrefix(sub)
	if msub != msub.Masked() || msub.Bits() != 28 || !netip.MustParsePrefix("100.64.0.0/24").Contains(msub.Addr()) {
		t.Errorf("%s → %s", sub, msub)
	}
	if back, _ := m.unmaskPrefix(msub); back != sub {
		t.Errorf("unmask(mask(%s)) = %s", sub, back)
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2001:db8:5::1", "100.64.2.1"} {
		a := netip.MustParseAddr(s)
		if got, ok := m.maskAddr(a); ok || got != a {
			t.Errorf("mask(%s) = %s, %v; want unchanged", a, got, ok)
		}
		if got, ok := m.unmaskAddr(a); ok || got != a {
			t.Errorf("unmask(%s) = %s, %v; want unchanged", a, got, ok)
		}
	}
	if _, ok := m.maskPrefix(netip.MustParsePrefix("198.51.0.0/16")); ok {
		t.Error("a prefix wider than the network was masked")
	}
}

// TestPublicRangeLoad: broken network rules fail the load; a real address
// from a pseudonym range fails the input check with the hint.
func TestPublicRangeLoad(t *testing.T) {
	for _, bad := range []string{
		`{"networks": ["198.51.100.0/24", "198.51.100.128/25"]}`,
		`{"networks": ["198.51.0.0/16"], "public_range": "100.64.0.0/17"}`,
		`{"networks": ["3fff:1::/48"]}`,
	} {
		if _, err := ParseRules([]byte(bad)); err == nil {
			t.Errorf("loaded %s", bad)
		}
	}
	m := testMapper(t, `{"networks": ["2001:db8:4a1::/48"]}`)
	for _, s := range []string{"100.64.3.4", "3fff::1"} {
		err := m.checkInput(netip.MustParseAddr(s))
		var re *RejectError
		if !errors.As(err, &re) {
			t.Fatalf("checkInput(%s) = %v, want a RejectError", s, err)
		}
		// Diagnostics may be returned to the model: do not repeat the real input.
		if want := "адрес из диапазона псевдонимов; смените public_range или добавьте в allow"; re.Reason != want {
			t.Errorf("reason %q, want %q", re.Reason, want)
		}
	}
	for _, s := range []string{"8.8.8.8", "10.1.2.3", "2001:db8:4a1::1"} {
		if err := m.checkInput(netip.MustParseAddr(s)); err != nil {
			t.Errorf("checkInput(%s) = %v", s, err)
		}
	}
}

// TestEUI64: a link-local address built from a MAC stays built from the
// masked MAC; other interface IDs never turn into EUI-64 ones; MAC notation
// is kept.
func TestEUI64(t *testing.T) {
	m := testMapper(t, `{}`)
	lla := netip.MustParseAddr("fe80::200:5eff:fe00:5301")
	mac, ok := parseMAC("00:00:5e:00:53:01")
	if !ok {
		t.Fatal("parseMAC failed")
	}
	ml, ok := m.maskAddr(lla)
	if !ok || !netip.MustParsePrefix("fe80::/10").Contains(ml) {
		t.Fatalf("%s → %s", lla, ml)
	}
	iid := ml.As16()
	if want := eui64(m.maskMAC(mac)); !bytes.Equal(iid[8:], want[:]) {
		t.Fatalf("interface ID %x, want EUI-64 of the masked MAC %x", iid[8:], want)
	}
	if back, _ := m.unmaskAddr(ml); back != lla {
		t.Fatalf("unmask(mask(%s)) = %s", lla, back)
	}
	if m.unmaskMAC(m.maskMAC(mac)) != mac {
		t.Fatal("unmaskMAC(maskMAC(mac)) != mac")
	}

	rng := rand.New(rand.NewChaCha8([32]byte{4}))
	for i := range 1000 {
		a := randIn(rng, netip.MustParsePrefix("fe80::/10"))
		if i == 0 {
			a = netip.MustParseAddr("fe80::1")
		}
		b := a.As16()
		if isEUI64(b[8:]) {
			continue
		}
		ma, _ := m.maskAddr(a)
		mb := ma.As16()
		if isEUI64(mb[8:]) {
			t.Fatalf("%s → %s: an EUI-64 interface ID from a non-EUI one", a, ma)
		}
		if back, _ := m.unmaskAddr(ma); back != a {
			t.Fatalf("unmask(mask(%s)) = %s", a, back)
		}
	}

	zoned := netip.MustParseAddr("fe80::1%eth0")
	if mz, _ := m.maskAddr(zoned); mz.Zone() != "eth0" {
		t.Errorf("zone lost: %s", mz)
	}

	for _, s := range []string{"00:00:5e:00:53:01", "00-00-5E-00-53-2A", "0000.5e00.5321", "00:00:5E:00:53:44"} {
		mac, ok := parseMAC(s)
		if !ok {
			t.Fatalf("parseMAC(%q) failed", s)
		}
		masked := m.maskMAC(mac)
		out := formatMACLike(s, masked)
		if len(out) != len(s) {
			t.Fatalf("%q → %q", s, out)
		}
		for i := range s {
			if !isHex(s[i]) && out[i] != s[i] {
				t.Fatalf("%q → %q: separator moved", s, out)
			}
		}
		if upper := strings.ToUpper(s) == s; upper != (strings.ToUpper(out) == out) || !upper && strings.ToLower(out) != out {
			t.Fatalf("%q → %q: letter case changed", s, out)
		}
		if again, ok := parseMAC(out); !ok || again != masked {
			t.Fatalf("parseMAC(%q) = %x, want %x", out, again, masked)
		}
	}
	for _, s := range []string{"00:00:5e:00:53", "0000.5e00.53", "00:00-5e:00:53:01", "00:00:5g:00:53:01"} {
		if _, ok := parseMAC(s); ok {
			t.Errorf("parseMAC(%q) accepted", s)
		}
	}
}
