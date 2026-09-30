package privacy

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// legacyNamespace is a profile directory of an earlier key, the shape
// Snapshot builds: "<profile>-<64 hex>".
var legacyNamespace = "p1-" + strings.Repeat("ab", 32)

func writeLegacyFixture(t *testing.T, root string, rel ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{root}, rel...)...)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"real":"synthetic-legacy-value"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCleanLegacyOnSnapshot(t *testing.T) {
	home := t.TempDir()
	runtimeConfig(t, home, `{}`)
	old := writeLegacyFixture(t, filepath.Join(home, "privacy-runtime"), legacyNamespace, "privacy", "sessions", "v2", "abc.jsonl")
	if _, err := os.Lstat(old); err != nil {
		t.Fatalf("fixture missing before Snapshot: %v", err)
	}
	rt := NewRuntime(home)
	if _, err := rt.Snapshot(); err != nil {
		t.Fatal(err)
	}
	if state := rt.State(); state.LegacyRemoved < 1 || state.LegacyUnknown != 0 {
		t.Fatalf("state = %d removed, %d unknown; want ≥1, 0", state.LegacyRemoved, state.LegacyUnknown)
	}
	if _, err := os.Lstat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy file after Snapshot: %v; want it removed before the first Mask", err)
	}
	if _, err := os.Lstat(filepath.Join(home, "privacy-runtime", legacyNamespace)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("emptied namespace after Snapshot: %v; want it removed", err)
	}
}

func TestCleanLegacy(t *testing.T) {
	type fixture struct {
		root string
		run  func() (removed, unknown int, err error)
	}
	setup := func(t *testing.T, rel ...string) (fixture, string) {
		t.Helper()
		root := filepath.Join(t.TempDir(), "privacy-runtime")
		path := writeLegacyFixture(t, root, rel...)
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("fixture missing before the call: %v", err)
		}
		return fixture{root: root, run: func() (int, int, error) { return cleanLegacy(root) }}, path
	}
	gone := func(t *testing.T, path string) {
		t.Helper()
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: %v; want it removed", path, err)
		}
	}
	kept := func(t *testing.T, path string) {
		t.Helper()
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("%s: %v; want it kept", path, err)
		}
	}

	t.Run("v2 file removed", func(t *testing.T) {
		f, path := setup(t, legacyNamespace, "privacy", "sessions", "v2", "abc.jsonl")
		removed, unknown, err := f.run()
		if err != nil || removed < 1 || unknown != 0 {
			t.Fatalf("cleanLegacy = %d, %d, %v; want ≥1, 0, nil", removed, unknown, err)
		}
		gone(t, path)
	})
	t.Run("v1 file removed", func(t *testing.T) {
		f, path := setup(t, legacyNamespace, "privacy", "sessions", "abc.jsonl")
		removed, unknown, err := f.run()
		if err != nil || removed < 1 || unknown != 0 {
			t.Fatalf("cleanLegacy = %d, %d, %v; want ≥1, 0, nil", removed, unknown, err)
		}
		gone(t, path)
	})
	t.Run("lock removed", func(t *testing.T) {
		f, path := setup(t, legacyNamespace, "privacy", "sessions", ".lock")
		v2lock := writeLegacyFixture(t, f.root, legacyNamespace, "privacy", "sessions", "v2", ".lock")
		removed, unknown, err := f.run()
		if err != nil || removed != 2 || unknown != 0 {
			t.Fatalf("cleanLegacy = %d, %d, %v; want 2, 0, nil", removed, unknown, err)
		}
		gone(t, path)
		gone(t, v2lock)
	})
	t.Run("atomic leftover removed", func(t *testing.T) {
		f, path := setup(t, legacyNamespace, "privacy", "sessions", ".abc.jsonl.123456")
		removed, unknown, err := f.run()
		if err != nil || removed != 1 || unknown != 0 {
			t.Fatalf("cleanLegacy = %d, %d, %v; want 1, 0, nil", removed, unknown, err)
		}
		gone(t, path)
	})
	t.Run("empty dirs removed", func(t *testing.T) {
		f, _ := setup(t, legacyNamespace, "privacy", "sessions", "v2", "abc.jsonl")
		writeLegacyFixture(t, f.root, legacyNamespace, "privacy", "sessions", "def.jsonl")
		if removed, _, err := f.run(); err != nil || removed != 2 {
			t.Fatalf("cleanLegacy = %d, %v; want 2, nil", removed, err)
		}
		gone(t, filepath.Join(f.root, legacyNamespace))
		kept(t, f.root)
	})
	t.Run("v3 untouched", func(t *testing.T) {
		f, legacy := setup(t, legacyNamespace, "privacy", "sessions", "abc.jsonl")
		v3 := writeLegacyFixture(t, f.root, "sessions", "v3", "s1.jsonl")
		lock := writeLegacyFixture(t, f.root, "sessions", ".lock")
		removed, unknown, err := f.run()
		if err != nil || removed != 1 || unknown != 0 {
			t.Fatalf("cleanLegacy = %d, %d, %v; want 1, 0, nil", removed, unknown, err)
		}
		gone(t, legacy)
		kept(t, v3)
		kept(t, lock)
	})
	t.Run("unknown file kept and counted", func(t *testing.T) {
		f, legacy := setup(t, legacyNamespace, "privacy", "sessions", "abc.jsonl")
		inside := writeLegacyFixture(t, f.root, legacyNamespace, "privacy", "sessions", "notes.txt")
		top := writeLegacyFixture(t, f.root, "other.txt")
		deep := writeLegacyFixture(t, f.root, legacyNamespace, "privacy", "sessions", "v2", "x", "abc.jsonl")
		removed, unknown, err := f.run()
		if err != nil || removed != 1 || unknown != 3 {
			t.Fatalf("cleanLegacy = %d, %d, %v; want 1, 3, nil", removed, unknown, err)
		}
		gone(t, legacy)
		kept(t, inside)
		kept(t, top)
		kept(t, deep)
	})
	t.Run("symlink not followed", func(t *testing.T) {
		outside := t.TempDir()
		target := writeLegacyFixture(t, outside, "privacy", "sessions", "abc.jsonl")
		f, legacy := setup(t, legacyNamespace, "privacy", "sessions", "abc.jsonl")
		link := filepath.Join(f.root, "p2-"+strings.Repeat("cd", 32))
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlink: %v", err)
		}
		fileLink := filepath.Join(f.root, legacyNamespace, "privacy", "sessions", "v2", "link.jsonl")
		if err := os.MkdirAll(filepath.Dir(fileLink), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, fileLink); err != nil {
			t.Fatal(err)
		}
		removed, unknown, err := f.run()
		if err != nil || removed != 1 || unknown != 2 {
			t.Fatalf("cleanLegacy = %d, %d, %v; want 1, 2, nil", removed, unknown, err)
		}
		gone(t, legacy)
		kept(t, link)
		kept(t, fileLink)
		kept(t, target)
	})
	t.Run("failure keeps going", func(t *testing.T) {
		f, stuck := setup(t, legacyNamespace, "privacy", "sessions", "abc.jsonl")
		next := writeLegacyFixture(t, f.root, "p3-"+strings.Repeat("ef", 32), "privacy", "sessions", "abc.jsonl")
		readOnlyDir(t, filepath.Dir(stuck))
		removed, _, err := f.run()
		if err == nil || removed != 1 {
			t.Fatalf("cleanLegacy = %d, %v; want 1 and the error", removed, err)
		}
		kept(t, stuck)
		gone(t, next)
	})
	t.Run("missing runtime dir", func(t *testing.T) {
		removed, unknown, err := cleanLegacy(filepath.Join(t.TempDir(), "privacy-runtime"))
		if err != nil || removed != 0 || unknown != 0 {
			t.Fatalf("cleanLegacy = %d, %d, %v; want 0, 0, nil", removed, unknown, err)
		}
	})
}

// readOnlyDir makes dir refuse removals of its entries until the test ends or
// the returned func restores it.
func readOnlyDir(t *testing.T, dir string) func() {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not stop removal here")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	restore := func() { _ = os.Chmod(dir, 0o700) }
	t.Cleanup(restore)
	return restore
}

func TestCleanLegacyFailureVisible(t *testing.T) {
	var logs bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })

	home := t.TempDir()
	runtimeConfig(t, home, `{}`)
	old := writeLegacyFixture(t, filepath.Join(home, "privacy-runtime"), legacyNamespace, "privacy", "sessions", "abc.jsonl")
	restore := readOnlyDir(t, filepath.Dir(old))
	rt := NewRuntime(home)
	if _, err := rt.Snapshot(); err != nil {
		t.Fatalf("Snapshot = %v; a failed cleanup must not stop traffic", err)
	}
	if _, err := os.Lstat(old); err != nil {
		t.Fatalf("fixture: %v; want it still there after a refused removal", err)
	}
	line := logs.String()
	if !strings.Contains(line, "legacy cleanup failed") {
		t.Fatalf("router.log = %q; want the failure named", line)
	}
	for _, leak := range []string{home, "abc", "synthetic-legacy-value", legacyNamespace} {
		if strings.Contains(line, leak) {
			t.Fatalf("router.log = %q; carries %q", line, leak)
		}
	}
	if state := rt.State(); state.LegacyErrors != 1 {
		t.Fatalf("LegacyErrors = %d; want 1", state.LegacyErrors)
	}

	restore()
	runtimeConfig(t, home, `{"filters":{"phone":false}}`)
	if _, err := rt.Snapshot(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture after the next policy: %v; want the retry to remove it", err)
	}
	if state := rt.State(); state.LegacyErrors != 1 || state.LegacyRemoved < 1 {
		t.Fatalf("state = %d errors, %d removed; want 1, ≥1", state.LegacyErrors, state.LegacyRemoved)
	}
}
