package platform

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
