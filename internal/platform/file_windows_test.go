package platform

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// replaceFixture makes dst with old content, src with new, and opens dst the
// way a concurrent reader would.
func replaceFixture(t *testing.T) (src, dst string, reader *os.File) {
	t.Helper()
	dir := t.TempDir()
	src, dst = filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	return src, dst, reader
}

// Windows refuses a rename over a file another handle has open; ReplaceFile
// waits for the reader to close it.
func TestReplaceFileWaitsForReader(t *testing.T) {
	src, dst, reader := replaceFixture(t)
	time.AfterFunc(100*time.Millisecond, func() { reader.Close() })
	if err := ReplaceFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "new" {
		t.Fatalf("dst %q %v", got, err)
	}
}

func TestReplaceFileGivesUpWhileReaderStays(t *testing.T) {
	src, dst, _ := replaceFixture(t)
	start := time.Now()
	if err := ReplaceFile(src, dst); err == nil {
		t.Fatal("replaced a file another handle holds open")
	}
	if waited := time.Since(start); waited < replaceWait {
		t.Fatalf("gave up after %v, want %v", waited, replaceWait)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "old" {
		t.Fatalf("dst %q %v", got, err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("src: %v", err)
	}
}

// Access denied on a directory or a read-only file does not pass by waiting,
// so only a writable file's refusal counts as busy.
func TestRenameBusyOnlyWhileTargetOpen(t *testing.T) {
	dir := t.TempDir()
	file, readOnly := filepath.Join(dir, "file"), filepath.Join(dir, "read-only")
	for _, path := range []string{file, readOnly} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(readOnly, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // lets TempDir remove it
		if err := os.Chmod(readOnly, 0o600); err != nil {
			t.Error(err)
		}
	})
	denied := &os.LinkError{Op: "rename", Err: windows.ERROR_ACCESS_DENIED}
	for dst, want := range map[string]bool{file: true, readOnly: false, dir: false} {
		if got := renameBusy(denied, dst); got != want {
			t.Errorf("access denied on %s: busy = %v, want %v", filepath.Base(dst), got, want)
		}
	}
	sharing := &os.LinkError{Op: "rename", Err: windows.ERROR_SHARING_VIOLATION}
	if !renameBusy(sharing, dir) {
		t.Error("sharing violation: not busy")
	}
}
