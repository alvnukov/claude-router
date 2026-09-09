package main

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// History persists across restarts as one JSON line per finished request in
// ROUTER_UI_HISTORY_FILE (default history.jsonl next to the binary, mode 0600:
// it holds prompts). Pending requests are never written. The file is compacted
// from the ring once it holds more than twice the ring size.

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
	if compact {
		s.rewrite()
	}
}

// rewrite replaces the file with the finished records in the ring.
// Caller holds s.fmu.
func (s *store) rewrite() {
	s.mu.RLock()
	recs := make([]*record, 0, len(s.recs))
	for _, r := range s.recs {
		if r.Done() {
			recs = append(recs, r)
		}
	}
	s.mu.RUnlock()
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		log.Printf("history: %v", err)
		return
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			log.Printf("history: %v", err)
		}
	}
	if err := w.Flush(); err != nil {
		log.Printf("history: %v", err)
	}
	if err := f.Close(); err != nil {
		log.Printf("history: %v", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("history: %v", err)
		return
	}
	s.mu.Lock()
	s.lines = len(recs)
	s.mu.Unlock()
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
