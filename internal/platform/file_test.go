package platform

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// entries lists the names in dir, to catch temporary files left behind.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

func TestWriteFileAtomicCreatesAndReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	for _, want := range []string{"one", "two"} {
		if err := WriteFileAtomic(path, []byte(want), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("content %q %v, want %q", got, err, want)
		}
	}
	if names := entries(t, dir); len(names) != 1 || names[0] != "state.json" {
		t.Fatalf("directory holds %v", names)
	}
}

// A failed replace leaves what was at path and no temporary file.
func TestWriteFileAtomicFailureKeepsTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("new"), 0o600); err == nil {
		t.Fatal("replaced a directory")
	}
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		t.Fatalf("target: %v %v", fi, err)
	}
	if names := entries(t, dir); len(names) != 1 {
		t.Fatalf("directory holds %v", names)
	}
}

func TestWriteFileAtomicNeedsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "state.json")
	if err := WriteFileAtomic(path, []byte("x"), 0o600); err == nil {
		t.Fatal("wrote into a missing directory")
	}
}

func TestRenameRetryingRetriesWhileBusy(t *testing.T) {
	errBusy := errors.New("busy")
	calls := 0
	rename := func(src, dst string) error {
		if src != "src" || dst != "dst" {
			t.Fatalf("rename(%q, %q)", src, dst)
		}
		calls++
		if calls < 3 {
			return errBusy
		}
		return nil
	}
	busy := func(err error, _ string) bool { return err == errBusy }
	if err := renameRetrying(rename, busy, time.Minute, "src", "dst"); err != nil || calls != 3 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestRenameRetryingReturnsOtherErrorsAtOnce(t *testing.T) {
	errOther := errors.New("other")
	calls := 0
	rename := func(string, string) error { calls++; return errOther }
	busy := func(error, string) bool { return false }
	if err := renameRetrying(rename, busy, time.Minute, "src", "dst"); err != errOther || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestRenameRetryingGivesUpAfterWait(t *testing.T) {
	errBusy := errors.New("busy")
	calls := 0
	rename := func(string, string) error { calls++; return errBusy }
	busy := func(error, string) bool { return true }
	if err := renameRetrying(rename, busy, 0, "src", "dst"); err != errBusy || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
