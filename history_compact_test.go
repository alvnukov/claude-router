package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHistoryCompactionReadsBothWritersAfterDrain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	old, next := newStore(3, path), newStore(3, path)
	old.life, next.life = newLifecycle(false), newLifecycle(false)
	for i := range 12 {
		s := old
		if i%2 == 1 {
			s = next
		}
		s.persist(&record{ID: fmt.Sprintf("record-%d", i), End: time.Now()})
	}
	if err := next.compactAfterDrain(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("compacted history has %d lines; want newest 3", len(lines))
	}
	for i, line := range lines {
		var r record
		if err := json.Unmarshal([]byte(line), &r); err != nil || r.ID != fmt.Sprintf("record-%d", i+9) {
			t.Fatalf("compacted record %d: %v %+v", i, err, r)
		}
	}
}
