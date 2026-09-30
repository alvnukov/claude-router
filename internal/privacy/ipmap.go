package privacy

import (
	"encoding/hex"
	"net"
	"net/netip"
	"strings"
	"sync"

	"localrouter/internal/privacy/cryptopan"
)

// RejectError identifies input which cannot safely cross the privacy boundary.
type RejectError struct{ Reason, Path string }

func (e *RejectError) Error() string {
	if e.Path != "" {
		return "privacy: " + e.Path + ": " + e.Reason
	}
	return "privacy: " + e.Reason
}

type ipMapper struct {
	pan    *cryptopan.Cryptopan
	rules  *Rules
	blocks []netBlock
}

// prfSelfTest checks the PRF on its known answers once per process.
var prfSelfTest = sync.OnceValue(cryptopan.SelfTest)

// newIPMapper refuses the key when the PRF fails its known answers, so a
// broken build never masks.
func newIPMapper(key []byte, r *Rules) (*ipMapper, error) {
	if err := prfSelfTest(); err != nil {
		return nil, err
	}
	pan, err := cryptopan.NewV2(key)
	if err != nil {
		return nil, err
	}
	blocks := append([]netBlock(nil), r.blocks...)
	for _, p := range maskedClasses {
		blocks = append(blocks, netBlock{p, p})
	}
	return &ipMapper{pan: pan, rules: r, blocks: blocks}, nil
}

func copyBits(dst, src []byte, n int) {
	for i := 0; n > 0; i++ {
		k := min(n, 8)
		mask := byte(0xff << (8 - k))
		dst[i] = dst[i]&^mask | src[i]&mask
		n -= k
	}
}

// Preserve the input prefix while permuting only its subtree. For inverse
// traversal reconstruct the encrypted prefix before invoking Crypto-PAn.
func (m *ipMapper) permute(b []byte, prefix int, inverse bool) []byte {
	if !inverse {
		out := m.pan.AnonymizeBytes(b)
		copyBits(out, b, prefix)
		return out
	}
	head := m.pan.AnonymizeBytes(b)
	in := append([]byte(nil), b...)
	copyBits(in, head, prefix)
	out := m.pan.DeanonymizeBytes(in)
	copyBits(out, b, prefix)
	return out
}

func (m *ipMapper) maskAddr(a netip.Addr) (netip.Addr, bool)   { return m.address(a, false) }
func (m *ipMapper) unmaskAddr(a netip.Addr) (netip.Addr, bool) { return m.address(a, true) }

// embedded4 returns the IPv4 address an IPv6 one carries, mapped
// (::ffff:a.b.c.d) or compatible (::a.b.c.d, but not :: or ::1), in whatever
// notation it is written. Such an address is masked exactly
// as the bare IPv4.
func embedded4(a netip.Addr) (netip.Addr, bool) {
	if a.Is4In6() {
		return a.Unmap(), true
	}
	b := a.As16()
	if a.Is6() && !a.IsUnspecified() && !a.IsLoopback() && [12]byte(b[:12]) == [12]byte{} {
		return netip.AddrFrom4([4]byte(b[12:])), true
	}
	return a, false
}

// rewrap puts v4 back into the IPv6 form of a.
func rewrap(a, v4 netip.Addr) netip.Addr {
	b := a.As16()
	copy(b[12:], v4.AsSlice())
	return netip.AddrFrom16(b).WithZone(a.Zone())
}

func (m *ipMapper) address(a netip.Addr, inverse bool) (netip.Addr, bool) {
	if v4, ok := embedded4(a); ok {
		out, ok := m.address(v4, inverse)
		return rewrap(a, out), ok
	}
	for _, block := range m.blocks {
		from, to := block.Real, block.Pseudo
		if inverse {
			from, to = to, from
		}
		if !from.Contains(a.WithZone("")) {
			continue
		}
		b := a.AsSlice()
		// Both directions use the original network as the cipher's context.
		copyBits(b, block.Real.Addr().AsSlice(), from.Bits())
		if block.Real == netip.MustParsePrefix("fe80::/10") {
			b = append(m.permute(b[:8], 10, inverse), m.interfaceID(b[8:], inverse)...)
		} else {
			b = m.permute(b, from.Bits(), inverse)
		}
		copyBits(b, to.Addr().AsSlice(), to.Bits())
		out, _ := netip.AddrFromSlice(b)
		return out.WithZone(a.Zone()), true
	}
	return a, false
}

func (m *ipMapper) maskPrefix(p netip.Prefix) (netip.Prefix, bool)   { return m.prefix(p, false) }
func (m *ipMapper) unmaskPrefix(p netip.Prefix) (netip.Prefix, bool) { return m.prefix(p, true) }

func (m *ipMapper) prefix(p netip.Prefix, inverse bool) (netip.Prefix, bool) {
	if v4, ok := embedded4(p.Addr()); ok && p.Bits() >= 96 {
		out, ok := m.prefix(netip.PrefixFrom(v4, p.Bits()-96), inverse)
		return netip.PrefixFrom(rewrap(p.Addr(), out.Addr()), out.Bits()+96), ok
	}
	for _, b := range m.blocks {
		from := b.Real
		if inverse {
			from = b.Pseudo
		}
		if !from.Contains(p.Addr()) || p.Bits() < from.Bits() {
			continue
		}
		a, _ := m.address(p.Addr(), inverse)
		z := p.Masked().Addr()
		hz, _ := m.address(z, inverse)
		targetZero := netip.PrefixFrom(hz, p.Bits()).Masked().Addr()
		// Swap the two images so that a canonical network stays canonical,
		// without losing any host address. This is a CIDR-only permutation.
		if p.Addr() == z {
			a = targetZero
		} else if a == targetZero {
			a = hz
		}
		return netip.PrefixFrom(a, p.Bits()), true
	}
	return p, false
}

// minFreeBits is the threshold k >= 8. A pseudonym inside a masked class
// keeps the class open and hides the k = L - bits(class) bits after it, no
// more; below a whole octet the value would be guessed better than 1/256, so
// it leaves as a placeholder without Crypto-PAn. The trade-off: at least 8
// bits are hidden; how guessable a value is from a dictionary inside the
// prefix it keeps is neither measured nor promised.
const minFreeBits = 8

// freeBits returns the k bits the pseudonym of value hides: the length after
// the block for an address or a network, 24 for a MAC. A declared network
// replaces its block as well, so the threshold does not apply there (under
// is false). ok is false when no block handles value.
func (m *ipMapper) freeBits(value string) (k int, under, ok bool) {
	if _, ok := parseMAC(value); ok {
		return 24, false, true
	}
	var a netip.Addr
	bits := 0
	if p, err := netip.ParsePrefix(value); err == nil {
		a, bits = p.Addr(), p.Bits()
	} else if a, err = netip.ParseAddr(value); err == nil {
		a, bits = a.WithZone(""), a.BitLen()
	} else {
		return 0, false, false
	}
	if v4, ok := embedded4(a); ok && bits >= 96 {
		a, bits = v4, bits-96
	}
	for _, b := range m.blocks {
		if b.Real.Contains(a) && bits >= b.Real.Bits() {
			k = bits - b.Real.Bits()
			return k, b.Real == b.Pseudo && k < minFreeBits, true
		}
	}
	return 0, false, false
}

func (m *ipMapper) maskMAC(a [6]byte) [6]byte   { return [6]byte(m.permute(a[:], 24, false)) }
func (m *ipMapper) unmaskMAC(a [6]byte) [6]byte { return [6]byte(m.permute(a[:], 24, true)) }
func isEUI64(b []byte) bool                     { return len(b) == 8 && b[3] == 0xff && b[4] == 0xfe }
func eui64(a [6]byte) [8]byte                   { return [8]byte{a[0] ^ 2, a[1], a[2], 0xff, 0xfe, a[3], a[4], a[5]} }

func (m *ipMapper) interfaceID(b []byte, inverse bool) []byte {
	if isEUI64(b) {
		a := [6]byte{b[0] ^ 2, b[1], b[2], b[5], b[6], b[7]}
		if inverse {
			a = m.unmaskMAC(a)
		} else {
			a = m.maskMAC(a)
		}
		out := eui64(a)
		return out[:]
	}
	// Cycle walking keeps non-EUI-64 IDs in their own domain, bijectively.
	for {
		b = m.permute(b, 0, inverse)
		if !isEUI64(b) {
			return b
		}
	}
}

func (m *ipMapper) checkInput(a netip.Addr) error {
	if m.rules.PublicRange.Contains(a) || m.rules.PublicRange6.Contains(a) {
		return &RejectError{Reason: "адрес из диапазона псевдонимов; смените public_range или добавьте в allow"}
	}
	return nil
}

func parseMAC(s string) ([6]byte, bool) {
	b, err := net.ParseMAC(s)
	if err != nil || len(b) != 6 {
		return [6]byte{}, false
	}
	return [6]byte(b), true
}
func isHex(b byte) bool { return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F' }
func formatMACLike(s string, mac [6]byte) string {
	h := hex.EncodeToString(mac[:])
	if s == strings.ToUpper(s) {
		h = strings.ToUpper(h)
	}
	out, j := []byte(s), 0
	for i := range out {
		if isHex(out[i]) {
			out[i] = h[j]
			j++
		}
	}
	return string(out)
}
