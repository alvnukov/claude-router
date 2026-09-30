package privacy

import (
	"container/list"
	"sync"
	"time"
)

// Pseudonym pairs live only in process memory: the real value never reaches
// disk. A session keeps at most pairLimit pairs, dropping the one used least
// recently, and a pair not used for pairTTL is gone. The lifetime counts from
// the last use because sessions run longer than a day.
const (
	pairLimit = 20000
	pairTTL   = 24 * time.Hour
	pairSweep = time.Minute
)

type pair struct {
	pseudo, real string
	kind         Kind
	used         time.Time
}

// sessionPairs orders its pairs by use, the most recent at the front.
type sessionPairs struct {
	byPseudo map[string]*list.Element
	order    list.List
}

type pairStore struct {
	mu       sync.Mutex
	sessions map[string]*sessionPairs
	limit    int
	ttl      time.Duration
	swept    time.Time
}

var pairStores sync.Map

func newPairStore() *pairStore {
	return &pairStore{sessions: make(map[string]*sessionPairs), limit: pairLimit, ttl: pairTTL}
}

// sharedPairs returns the one pair store of dir, as sessionLocks does for
// session files: every engine on the same directory sees the same pairs.
func sharedPairs(dir string) *pairStore {
	p, _ := pairStores.LoadOrStore(dir, newPairStore())
	return p.(*pairStore)
}

// put records that pseudo stands for real in session. A pseudonym already
// standing for another value is a collision: the first pair stays.
func (p *pairStore) put(session, pseudo, real string, kind Kind, now time.Time) (collision bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now.Sub(p.swept) >= pairSweep {
		p.sweep(now)
	}
	s := p.sessions[session]
	if s == nil {
		s = &sessionPairs{byPseudo: make(map[string]*list.Element)}
		p.sessions[session] = s
	}
	if e := s.byPseudo[pseudo]; e != nil {
		old := e.Value.(*pair)
		if old.real != real || old.kind != kind {
			return true
		}
		old.used = now
		s.order.MoveToFront(e)
		return false
	}
	s.byPseudo[pseudo] = s.order.PushFront(&pair{pseudo: pseudo, real: real, kind: kind, used: now})
	for s.order.Len() > p.limit {
		last := s.order.Back()
		delete(s.byPseudo, last.Value.(*pair).pseudo)
		s.order.Remove(last)
	}
	return false
}

// get returns the value pseudo stands for in session, if the pair is alive.
func (p *pairStore) get(session, pseudo string, now time.Time) (real string, kind Kind, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[session]
	if s == nil {
		return "", "", false
	}
	e := s.byPseudo[pseudo]
	if e == nil {
		return "", "", false
	}
	pr := e.Value.(*pair)
	if now.Sub(pr.used) > p.ttl {
		delete(s.byPseudo, pseudo)
		s.order.Remove(e)
		if s.order.Len() == 0 {
			delete(p.sessions, session)
		}
		return "", "", false
	}
	pr.used = now
	s.order.MoveToFront(e)
	return pr.real, pr.kind, true
}

// forget drops the pairs of session, or of every session when it is "".
func (p *pairStore) forget(session string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if session == "" {
		clear(p.sessions)
		return
	}
	delete(p.sessions, session)
}

// sweep drops the pairs of every session idle past the lifetime, and the
// sessions left empty. Pairs are ordered by use, so each session is walked
// from its back only as far as its first live pair.
func (p *pairStore) sweep(now time.Time) {
	p.swept = now
	for id, s := range p.sessions {
		for e := s.order.Back(); e != nil && now.Sub(e.Value.(*pair).used) > p.ttl; e = s.order.Back() {
			delete(s.byPseudo, e.Value.(*pair).pseudo)
			s.order.Remove(e)
		}
		if s.order.Len() == 0 {
			delete(p.sessions, id)
		}
	}
}
