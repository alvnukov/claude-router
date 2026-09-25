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

// perm is the file's mode whatever the umask, also when it replaces a file
// with another mode.
func TestWriteFileAtomicSetsPerm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	for _, perm := range []os.FileMode{0o644, 0o600, 0o700} {
		if err := WriteFileAtomic(path, []byte("x"), perm); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != perm {
			t.Fatalf("mode %v %v, want %v", fi.Mode(), err, perm)
		}
	}
}
