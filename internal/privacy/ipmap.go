package privacy

import (
	"encoding/hex"
	"net"
	"net/netip"
	"strings"

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

func newIPMapper(key []byte, r *Rules, version int) (*ipMapper, error) {
	newPan := cryptopan.NewV2
	if version == prfV1 {
		newPan = cryptopan.New
	}
	pan, err := newPan(key)
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

func (m *ipMapper) address(a netip.Addr, inverse bool) (netip.Addr, bool) {
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
