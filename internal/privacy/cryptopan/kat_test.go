package cryptopan

import "testing"

// The same known answers that guard key initialization, run in CI.
func TestKAT(t *testing.T) {
	if err := SelfTest(); err != nil {
		t.Fatal(err)
	}
}
