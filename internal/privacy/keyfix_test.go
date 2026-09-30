package privacy

import (
	"crypto/sha256"
	"encoding/binary"
	"testing"
)

// fixedKey is the session key sha256(uint64 i). Mask tests pin their keys
// this way, so a case that depends on the key fails always or never.
func fixedKey(i uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], i)
	k := sha256.Sum256(b[:])
	return k[:]
}

func fixedKeyEngine(t testing.TB, r *Rules, key []byte) *Engine {
	t.Helper()
	e, err := Open(t.TempDir(), r, Options{Home: "/home/testlogin", Hostname: "test-host.local", newKey: func() ([]byte, error) {
		return append([]byte(nil), key...), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}
