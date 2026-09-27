package cryptopan

import (
	"bytes"
	"math/rand/v2"
	"net"
	"testing"
)

// TestDeanonymize: the inverse undoes Anonymize on the upstream vectors and on
// random values of every length the privacy package uses: MAC, IPv4, the
// upper or lower half of IPv6, IPv6.
func TestDeanonymize(t *testing.T) {
	cpan, err := New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"128.11.68.132", "135.242.180.132", "2001:db8::1"} {
		ip := net.ParseIP(v)
		if got := cpan.Deanonymize(cpan.Anonymize(ip)); !got.Equal(ip) {
			t.Errorf("Deanonymize(Anonymize(%s)) = %s", ip, got)
		}
		raw := ip.To4()
		if raw == nil {
			raw = ip.To16()
		}
		if got, want := cpan.AnonymizeBytes(raw), cpan.Anonymize(ip); !net.IP(got).Equal(want) {
			t.Errorf("AnonymizeBytes(%s) = %s, Anonymize = %s", ip, net.IP(got), want)
		}
	}
	rng := rand.New(rand.NewChaCha8([32]byte{1}))
	for _, n := range []int{4, 6, 8, 16} {
		for range 2000 {
			in := make([]byte, n)
			for i := range in {
				in[i] = byte(rng.Uint32())
			}
			out := cpan.AnonymizeBytes(in)
			if len(out) != n {
				t.Fatalf("AnonymizeBytes(%x) has %d bytes", in, len(out))
			}
			if back := cpan.DeanonymizeBytes(out); !bytes.Equal(back, in) {
				t.Fatalf("DeanonymizeBytes(AnonymizeBytes(%x)) = %x", in, back)
			}
		}
	}
	// A value of n bytes maps as the first n bytes of any longer one: the
	// privacy package relies on it to treat half an IPv6 address alone.
	full := net.ParseIP("fe80::200:5eff:fe00:5301").To16()
	if a, b := cpan.AnonymizeBytes(full[:8]), cpan.AnonymizeBytes(full)[:8]; !bytes.Equal(a, b) {
		t.Fatalf("upper half maps to %x alone, %x inside the address", a, b)
	}
}
