//go:build unix

package config

import (
	"os"
	"testing"
)

// requirePrivateFile fails unless path is a regular file readable only by its
// owner: the router keeps tokens and configuration at 0600.
func requirePrivateFile(t *testing.T, path string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 {
		t.Fatalf("%s: mode %v, want 0600", path, st.Mode())
	}
}
