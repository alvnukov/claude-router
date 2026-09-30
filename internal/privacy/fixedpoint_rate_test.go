package privacy

import (
	"math"
	"math/rand/v2"
	"net/netip"
	"os"
	"testing"

	"localrouter/internal/privacy/cryptopan"
)

// fixedPointRate masks one random value with k free bits under each of n
// random keys and returns the share the permutation left equal to itself.
// k = 8 is a /24 network in 192.168.0.0/16, k = 16 an address in it.
func fixedPointRate(t *testing.T, version, k, n int) float64 {
	t.Helper()
	rules, err := ParseRules([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(uint64(version), uint64(k)))
	key := make([]byte, 32)
	fixed := 0
	for range n {
		for i := range key {
			key[i] = byte(rng.Uint32())
		}
		pan, err := cryptopan.NewV2(key)
		if version == prfV1 {
			pan, err = cryptopan.New(key)
		}
		if err != nil {
			t.Fatal(err)
		}
		m := &ipMapper{pan: pan, rules: rules}
		for _, p := range maskedClasses {
			m.blocks = append(m.blocks, netBlock{p, p})
		}
		a := netip.AddrFrom4([4]byte{192, 168, byte(rng.Uint32()), byte(rng.Uint32())})
		if k == 8 {
			p := netip.PrefixFrom(a, 24).Masked()
			if out, ok := m.maskPrefix(p); ok && out == p {
				fixed++
			}
			continue
		}
		if out, ok := m.maskAddr(a); ok && out == a {
			fixed++
		}
	}
	return float64(fixed) / float64(n)
}

// The share of keys that leave a value with k
// free bits unchanged is within 3σ of 2^-k, k = 8 on 1e5 keys, k = 16 on 1e6
// keys (under PRIVACY_MEASURE, it takes longer than 2 s). Version 1 is
// measured alongside for comparison.
func TestFixedPointRate(t *testing.T) {
	cases := []struct{ k, n int }{{8, 100000}}
	if os.Getenv("PRIVACY_MEASURE") != "" {
		cases = append(cases, struct{ k, n int }{16, 1000000})
	}
	for _, c := range cases {
		p := math.Exp2(-float64(c.k))
		sigma := math.Sqrt(p * (1 - p) / float64(c.n))
		v1, v2 := fixedPointRate(t, prfV1, c.k, c.n), fixedPointRate(t, prfV2, c.k, c.n)
		t.Logf("k=%d n=%d 2^-k=%.3e σ=%.3e v1=%.3e (%+.1fσ) v2=%.3e (%+.1fσ)", c.k, c.n, p, sigma, v1, (v1-p)/sigma, v2, (v2-p)/sigma)
		if math.Abs(v2-p) > 3*sigma {
			t.Errorf("k=%d: version 2 fixed-point share %.3e, want 2^-k=%.3e within 3σ=%.3e", c.k, v2, p, 3*sigma)
		}
	}
}
