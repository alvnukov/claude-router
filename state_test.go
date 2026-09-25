package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateSnapshotRestoresBindingsAndRatings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := newHealth("")
	old.record("codex/model", true, 250*time.Millisecond, "")
	old.sessions = map[string]sessionBinding{"session": {Model: "codex/model", Selection: "hash", Used: time.Now()}}
	old.acquire("codex/model")
	if err := old.saveSnapshot(path); err != nil {
		t.Fatal(err)
	}
	requirePrivateFile(t, path)
	fresh := newHealth("")
	if err := fresh.loadSnapshot(path); err != nil {
		t.Fatal(err)
	}
	if fresh.snapshot("codex/model").OK != 1 || fresh.sessions["session"].Model != "codex/model" {
		t.Fatalf("snapshot not restored: %+v %+v", fresh.snapshot("codex/model"), fresh.sessions)
	}
	if fresh.load("codex/model") != 0 {
		t.Fatal("in-flight counts must not survive into the new process")
	}
}

func TestStateSnapshotMissingOrCorruptStartsCold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	h := newHealth("")
	if err := h.loadSnapshot(path); err != nil {
		t.Fatalf("missing snapshot: %v", err)
	}
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.loadSnapshot(path); err == nil {
		t.Fatal("corrupt snapshot should report error for logging")
	}
	if len(h.sessions) != 0 || len(h.m) != 0 {
		t.Fatal("corrupt snapshot changed cold state")
	}
}
