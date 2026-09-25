//go:build unix

package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMkdirPrivateIsOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	if err := MkdirPrivate(dir); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{dir, filepath.Dir(dir)} {
		if fi, err := os.Stat(d); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", d, fi.Mode(), err)
		}
	}
}
