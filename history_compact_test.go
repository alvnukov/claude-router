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

// A router compacts as it goes only while nothing else appends to the file,
// and then from the file, so lines another router appended survive.
func TestHistoryCompactsInlineOnlyWhenAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	other, active := newStore(3, path), newStore(3, path)
	other.life, active.life = newLifecycle(false), newLifecycle(false)
	other.life.drain()
	write := func(s *store, id string) { s.persist(&record{ID: id, End: time.Now()}) }
	lines := func() []string {
		data, _ := os.ReadFile(path)
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	for i := range 6 {
		write(active, fmt.Sprintf("active-%d", i))
	}
	write(other, "drained")
	write(active, "active-6")
	if got := len(lines()); got != 8 {
		t.Fatalf("compacted while another router may append: %d lines", got)
	}
	active.life.markAlone()
	write(active, "active-7")
	got := lines()
	if len(got) != 3 || !strings.Contains(got[0], `"drained"`) {
		t.Fatalf("alone router did not compact from the file: %q", got)
	}
	if err := active.life.quiesce(); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		write(active, fmt.Sprintf("quiesced-%d", i))
	}
	if got := len(lines()); got != 7 {
		t.Fatalf("quiesced router compacted: %d lines", got)
	}
}
