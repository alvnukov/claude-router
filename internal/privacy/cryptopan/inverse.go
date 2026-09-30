package cryptopan

import "net"

// Deanonymize is the inverse of Anonymize: Deanonymize(Anonymize(a)) == a.
func (ctx *Cryptopan) Deanonymize(addr net.IP) net.IP {
	if v4addr := addr.To4(); v4addr != nil {
		orig := ctx.DeanonymizeBytes(v4addr)
		return net.IPv4(orig[0], orig[1], orig[2], orig[3])
	} else if v6addr := addr.To16(); v6addr != nil {
		return net.IP(ctx.DeanonymizeBytes(v6addr))
	}

	panic("unsupported address type")
}

// AnonymizeBytes runs Crypto-PAn over a value of 1 to 16 bytes. Output bit i
// depends only on input bits 0..i, so the first n bytes of a longer value map
// exactly as those n bytes alone.
func (ctx *Cryptopan) AnonymizeBytes(b []byte) []byte {
	return append([]byte(nil), ctx.anonymize(net.IP(b))...)
}

// DeanonymizeBytes is the inverse of AnonymizeBytes. Bit i of the one-time
// pad depends only on original bits 0..i-1, so the original is recovered one
// bit at a time, most significant first.
func (ctx *Cryptopan) DeanonymizeBytes(b []byte) []byte {
	addrBits := uint(len(b) * 8)
	var obfsAddr, input, output, origAddr bitvector
	copy(obfsAddr[:], b)
	copy(input[:], ctx.pad[:])

	ctx.encrypt(&output, &input, 0)
	origAddr.SetBit(0, obfsAddr.Bit(0)^output.Bit(0))
	for pos := uint(1); pos < addrBits; pos++ {
		input.SetBit(pos-1, origAddr.Bit(pos-1))
		ctx.encrypt(&output, &input, pos)
		origAddr.SetBit(pos, obfsAddr.Bit(pos)^output.Bit(0))
	}
	return append([]byte(nil), origAddr[:len(b)]...)
}
