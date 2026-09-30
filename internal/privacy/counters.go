package privacy

import (
	"maps"
	"sync"
)

// Counters are the engine's running totals since Open. They hold counts only,
// never values or session ids.
type Counters struct {
	// Sessions counts the sessions masked by PRF version; a request-only
	// scope is a session of its own.
	Sessions map[int]int
}

type counters struct {
	mu       sync.Mutex
	seen     map[string]bool
	sessions map[int]int
}

func (c *counters) session(id string, version int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen, c.sessions = make(map[string]bool), make(map[int]int)
	}
	if id != "" {
		if c.seen[id] {
			return
		}
		c.seen[id] = true
	}
	c.sessions[version]++
}

// Counters returns a copy of the running totals.
func (e *Engine) Counters() Counters {
	e.counts.mu.Lock()
	defer e.counts.mu.Unlock()
	return Counters{Sessions: maps.Clone(e.counts.sessions)}
}
