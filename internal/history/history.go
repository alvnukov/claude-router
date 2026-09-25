package history

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"

	"localrouter/internal/platform"
)

// History persists across restarts as one JSON line per finished request in
// the file New was given (mode 0600: it holds prompts). Pending requests are
// never written. Once the file holds more than twice the ring size, a router
// that is alone on it keeps the newest ring-size lines; while two blue/green
// slots overlap, neither compacts.

// Gate says whether this router may compact the file as it writes: it may
// only while no other router appends to it.
type Gate interface {
	CompactsHistory() bool
}

// SetGate makes compaction as the store writes wait for g; without a gate the
// store compacts as the only writer. Call it before the store is used.
func (s *Store) SetGate(g Gate) { s.gate = g }

// load reads the file and keeps the newest max records in the ring.
func (s *Store) load() {
	if s.path == "" {
		return
	}
	f, err := os.Open(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("history: %v", err)
		}
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var recs []*Record
	lines := 0
	for sc.Scan() {
		lines++
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil || r.ID == "" {
			continue
		}
		if r.Session == "" { // written before sessions were tracked
			r.Session = SessionOf(r.ReqBody)
		}
		recs = append(recs, &r)
	}
	if err := sc.Err(); err != nil {
		log.Printf("history: %s: %v", s.path, err)
	}
	if len(recs) > s.max {
		recs = recs[len(recs)-s.max:]
	}
	s.mu.Lock()
	for i, r := range recs {
		r.Seq = int64(i + 1)
	}
	s.seq = int64(len(recs))
	s.recs = recs
	s.lines = lines
	s.mu.Unlock()
	if len(recs) > 0 {
		log.Printf("history: loaded %d requests from %s", len(recs), s.path)
	}
}

// Persist appends one finished record; called with a copy, outside s.mu.
func (s *Store) Persist(r *Record) {
	if s.path == "" {
		return
	}
	line, err := json.Marshal(r)
	if err != nil {
		log.Printf("history: %v", err)
		return
	}
	s.fmu.Lock()
	defer s.fmu.Unlock()
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("history: %v", err)
		return
	}
	_, werr := f.Write(append(line, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		log.Printf("history: %v", werr)
		return
	}
	s.mu.Lock()
	s.lines++
	compact := s.lines > 2*s.max
	s.mu.Unlock()
	if compact && (s.gate == nil || s.gate.CompactsHistory()) {
		if err := s.CompactAfterDrain(); err != nil {
			log.Printf("history: %v", err)
		}
	}
}

// CompactAfterDrain keeps the newest ring-size lines of the file, including
// those another router appended. It runs only once no other router appends:
// the legacy router always, a slot after the deploy retired the other one.
// The lock file keeps its inode across compaction.
func (s *Store) CompactAfterDrain() error {
	if s.path == "" {
		return nil
	}
	return platform.WithLock(context.Background(), s.path+".lock", func() error {
		f, err := os.Open(s.path)
		if errors.Is(err, os.ErrNotExist) {
			return nil // nothing was served yet
		}
		if err != nil {
			return err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		var lines [][]byte
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			var r struct{ ID string }
			if json.Unmarshal(line, &r) == nil && r.ID != "" {
				lines = append(lines, line)
				if len(lines) > s.max {
					lines = lines[1:]
				}
			}
		}
		err = sc.Err()
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		var kept bytes.Buffer
		for _, line := range lines {
			kept.Write(line)
			kept.WriteByte('\n')
		}
		if err := platform.WriteFileAtomic(s.path, kept.Bytes(), 0o600); err != nil {
			return err
		}
		s.mu.Lock()
		s.lines = len(lines)
		s.mu.Unlock()
		return nil
	})
}

// truncate empties the file; used by Clear.
func (s *Store) truncate() {
	if s.path == "" {
		return
	}
	s.fmu.Lock()
	defer s.fmu.Unlock()
	if err := os.WriteFile(s.path, nil, 0o600); err != nil && !os.IsNotExist(err) {
		log.Printf("history: %v", err)
	}
	s.mu.Lock()
	s.lines = 0
	s.mu.Unlock()
}
