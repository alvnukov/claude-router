package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConfigWatchPausesInQuiesceAndResumesOnRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte("ROUTER_LOCAL_BALANCE=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cs := newConfigStore(config{balance: 1}, "")
	cs.envPath = path
	cs.envMtime = mtime(path)
	life := newLifecycle(false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs.watch(ctx, 10*time.Millisecond, life)
	if err := life.quiesce(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("ROUTER_LOCAL_BALANCE=5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The watch sees an edit by its mtime, and Windows' clock can give both
	// writes the same one.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if got := cs.get().balance; got != 1 {
		t.Fatalf("quiesced config watch changed balance to %d", got)
	}
	if err := life.activate(); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for cs.get().balance != 5 {
		select {
		case <-deadline:
			t.Fatal("config watch did not resume after rollback")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
