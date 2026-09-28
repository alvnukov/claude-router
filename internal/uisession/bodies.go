package uisession

import "sync"

type bodyEntry struct {
	facts Facts
	seen  bool
}

// Bodies remembers the facts of each recorded request, so a UI refresh does
// not decode every request body in history again. Only the effort and the
// title candidate are kept, never the body. The zero value is ready to use.
type Bodies struct {
	mu      sync.Mutex
	entries map[string]*bodyEntry
}

// Facts returns the facts of the request id, decoding body on first sight.
// A request without an id is decoded every time.
func (c *Bodies) Facts(id string, body []byte) Facts {
	if id == "" {
		return ParseFacts(body)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[id]; e != nil {
		e.seen = true
		return e.facts
	}
	if c.entries == nil {
		c.entries = map[string]*bodyEntry{}
	}
	e := &bodyEntry{facts: ParseFacts(body), seen: true}
	c.entries[id] = e
	return e.facts
}

// Sweep forgets requests not asked about since the previous Sweep.
func (c *Bodies) Sweep() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, e := range c.entries {
		if !e.seen {
			delete(c.entries, id)
			continue
		}
		e.seen = false
	}
}
