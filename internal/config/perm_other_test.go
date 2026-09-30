//go:build !unix

package config

import (
	"os"
	"testing"
)

// requirePrivateFile only checks that path is a regular file: NTFS keeps no
// unix permission bits, and the user profile's ACL is what protects the file.
func requirePrivateFile(t *testing.T, path string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Mode().IsRegular() {
		t.Fatalf("%s: mode %v, want a regular file", path, st.Mode())
	}
}
