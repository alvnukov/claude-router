package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"

	"localrouter/internal/platform"
)

// History persists across restarts as one JSON line per finished request in
// ROUTER_UI_HISTORY_FILE (default history.jsonl next to the binary, mode 0600:
// it holds prompts). Pending requests are never written. Once the file holds
// more than twice the ring size, a router that is alone on it keeps the newest
// ring-size lines; while two blue/green slots overlap, neither compacts.

func historyPath() string {
	if v, ok := os.LookupEnv("ROUTER_UI_HISTORY_FILE"); ok {
		return v // "" disables
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "history.jsonl")
	}
	return "history.jsonl"
}

// load reads the file and keeps the newest max records in the ring.
func (s *store) load() {
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
	var recs []*record
	lines := 0
	for sc.Scan() {
		lines++
		var r record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil || r.ID == "" {
			continue
		}
		if r.Session == "" { // written before sessions were tracked
			r.Session = sessionOf(r.ReqBody)
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

// persist appends one finished record; called with a copy, outside s.mu.
func (s *store) persist(r *record) {
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
	if compact && (s.life == nil || s.life.compactsHistory()) {
		if err := s.compactAfterDrain(); err != nil {
			log.Printf("history: %v", err)
		}
	}
}

// compactAfterDrain keeps the newest ring-size lines of the file, including
// those another router appended. It runs only once no other router appends:
// the legacy router always, a slot after the deploy retired the other one.
// The lock file keeps its inode across compaction.
func (s *store) compactAfterDrain() error {
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
		tmp, err := os.CreateTemp(filepath.Dir(s.path), ".history-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		if err := tmp.Chmod(0o600); err != nil {
			tmp.Close()
			return err
		}
		for _, line := range lines {
			if _, err := tmp.Write(append(line, '\n')); err != nil {
				tmp.Close()
				return err
			}
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		if err := os.Rename(tmp.Name(), s.path); err != nil {
			return err
		}
		s.mu.Lock()
		s.lines = len(lines)
		s.mu.Unlock()
		return nil
	})
}

// truncate empties the file; used by clear.
func (s *store) truncate() {
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
