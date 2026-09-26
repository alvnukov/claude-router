package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gate stands in for the router lifecycle: it compacts only while alone.
type gate struct{ alone atomic.Bool }

func (g *gate) CompactsHistory() bool { return g.alone.Load() }

func fileLines(t *testing.T, path string) []string {
	t.Helper()
	data, _ := os.ReadFile(path)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestHistoryCompactionReadsBothWritersAfterDrain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	old, next := New(3, path), New(3, path)
	old.SetGate(&gate{})
	next.SetGate(&gate{})
	for i := range 12 {
		s := old
		if i%2 == 1 {
			s = next
		}
		s.Persist(&Record{ID: fmt.Sprintf("record-%d", i), End: time.Now()})
	}
	if err := next.CompactAfterDrain(); err != nil {
		t.Fatal(err)
	}
	lines := fileLines(t, path)
	if len(lines) != 3 {
		t.Fatalf("compacted history has %d lines; want newest 3", len(lines))
	}
	for i, line := range lines {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil || r.ID != fmt.Sprintf("record-%d", i+9) {
			t.Fatalf("compacted record %d: %v %+v", i, err, r)
		}
	}
}

// A router that never served a request has no history file; cutover and
// deploy compact it all the same.
func TestHistoryCompactionWithoutAFileHasNothingToDo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := New(3, path).CompactAfterDrain(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("compaction created the history: %v", err)
	}
}

// A router compacts as it goes only while its gate says nothing else appends
// to the file, and then from the file, so lines another router appended
// survive.
func TestHistoryCompactsInlineOnlyWhenAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	other, active := New(3, path), New(3, path)
	otherGate, activeGate := &gate{}, &gate{}
	other.SetGate(otherGate)
	active.SetGate(activeGate)
	write := func(s *Store, id string) { s.Persist(&Record{ID: id, End: time.Now()}) }
	for i := range 6 {
		write(active, fmt.Sprintf("active-%d", i))
	}
	write(other, "drained")
	write(active, "active-6")
	if got := len(fileLines(t, path)); got != 8 {
		t.Fatalf("compacted while another router may append: %d lines", got)
	}
	activeGate.alone.Store(true)
	write(active, "active-7")
	got := fileLines(t, path)
	if len(got) != 3 || !strings.Contains(got[0], `"drained"`) {
		t.Fatalf("alone router did not compact from the file: %q", got)
	}
	activeGate.alone.Store(false)
	for i := range 4 {
		write(active, fmt.Sprintf("quiesced-%d", i))
	}
	if got := len(fileLines(t, path)); got != 7 {
		t.Fatalf("router that is no longer alone compacted: %d lines", got)
	}
}

// Without a gate the store is the only writer, as the legacy router and the
// tests are, and compacts once the file holds twice its size.
func TestHistoryWithoutGateCompactsAsItWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	st := New(2, path)
	for i := range 5 {
		st.Persist(&Record{ID: fmt.Sprintf("r-%d", i), End: time.Now()})
	}
	if got := fileLines(t, path); len(got) != 2 || !strings.Contains(got[1], `"r-4"`) {
		t.Fatalf("store without a gate did not compact: %q", got)
	}
}

func TestTwoStoresDoNotLoseHistoryDuringCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	old, next := New(3, path), New(3, path)
	old.SetGate(&gate{})
	next.SetGate(&gate{})
	var wg sync.WaitGroup
	for _, s := range []*Store{old, next} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 30 {
				r := &Record{ID: fmt.Sprintf("%p-%d", s, i), End: time.Now()}
				s.mu.Lock()
				s.recs = append(s.recs, r)
				s.mu.Unlock()
				s.Persist(r)
			}
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, line := range fileLines(t, path) {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("corrupt JSONL record: %v", err)
		}
		seen[r.ID] = true
	}
	if len(seen) != 60 {
		t.Fatalf("history lost overlap records: got %d, want 60", len(seen))
	}
}
