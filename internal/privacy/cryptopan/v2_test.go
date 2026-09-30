package cryptopan

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

// An address whose bits all equal the pad leaves the version 1 AES input
// unchanged at every step, so the whole one-time pad is one repeated bit.
// Version 2 must break that run.
func TestV2PadAddressNotConstant(t *testing.T) {
	for _, n := range []int{4, 16} {
		v1, err := New(testKey)
		if err != nil {
			t.Fatal(err)
		}
		v2, err := NewV2(testKey)
		if err != nil {
			t.Fatal(err)
		}
		a := append([]byte(nil), v1.pad[:n]...)
		if !constantPad(a, v1.AnonymizeBytes(a)) {
			t.Fatalf("version 1 pad is not constant on the pad address (%d bytes)", n)
		}
		if constantPad(a, v2.AnonymizeBytes(a)) {
			t.Fatalf("version 2 pad is constant on the pad address (%d bytes)", n)
		}
	}
}

func constantPad(in, out []byte) bool {
	x := make([]byte, len(in))
	for i := range in {
		x[i] = in[i] ^ out[i]
	}
	return bytes.Equal(x, make([]byte, len(x))) || bytes.Equal(x, bytes.Repeat([]byte{0xff}, len(x)))
}

// Version 2 stays a prefix-preserving bijection with a working inverse.
func TestV2InverseAndPrefix(t *testing.T) {
	cpan, err := NewV2(testKey)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewChaCha8([32]byte{2}))
	differs := false
	for _, n := range []int{4, 6, 8, 16} {
		for range 2000 {
			a := make([]byte, n)
			for i := range a {
				a[i] = byte(rng.Uint32())
			}
			b := append([]byte(nil), a...)
			bit := rng.IntN(n * 8)
			b[bit/8] ^= 0x80 >> (bit % 8)
			ma, mb := cpan.AnonymizeBytes(a), cpan.AnonymizeBytes(b)
			if got := cpan.DeanonymizeBytes(ma); !bytes.Equal(got, a) {
				t.Fatalf("DeanonymizeBytes(AnonymizeBytes(%x)) = %x", a, got)
			}
			if shared := commonPrefix(ma, mb); shared != bit {
				t.Fatalf("%x and %x share %d bits, images share %d", a, b, bit, shared)
			}
			differs = differs || !bytes.Equal(ma, v1.AnonymizeBytes(a))
		}
	}
	if !differs {
		t.Fatal("version 2 maps every value as version 1")
	}
}

func commonPrefix(a, b []byte) int {
	for i := range a {
		if x := a[i] ^ b[i]; x != 0 {
			n := i * 8
			for x&0x80 == 0 {
				x <<= 1
				n++
			}
			return n
		}
	}
	return len(a) * 8
}
