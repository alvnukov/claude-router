package cryptopan

import (
	"bytes"
	"encoding/hex"
	"errors"
)

// Known answers of version 2 on the key 00 01 .. 1f, recorded when version 2
// was introduced: an IPv4 address, a MAC and an IPv6 address. Version 1
// gives other answers for all three, so the defect cannot come back unseen.
var katVectors = [][2]string{
	{"0a080311", "a049fae7"},
	{"0242ac110002", "afa1a50b2383"},
	{"fd001234000000000000000000000007", "727bc43ee7994e8241e1e7394a673bd8"},
}

// SelfTest checks version 2 against its known answers, both directions. A
// caller runs it before masking with a key and refuses to mask if it fails.
func SelfTest() error {
	key := make([]byte, Size)
	for i := range key {
		key[i] = byte(i)
	}
	ctx, err := NewV2(key)
	if err != nil {
		return err
	}
	for _, v := range katVectors {
		in, _ := hex.DecodeString(v[0])
		want, _ := hex.DecodeString(v[1])
		if got := ctx.AnonymizeBytes(in); !bytes.Equal(got, want) || !bytes.Equal(ctx.DeanonymizeBytes(got), in) {
			return errors.New("cryptopan: self-test failed on a known answer")
		}
	}
	return nil
}
