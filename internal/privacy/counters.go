package privacy

import (
	"log"
	"maps"
	"math"
	"sync"
)

// Counters are the engine's running totals since Open. They hold counts only,
// never values or session ids.
type Counters struct {
	// Sessions counts the sessions masked by PRF version; a request-only
	// scope is a session of its own.
	Sessions map[int]int
	// Permuted counts distinct values of a permutation class (address,
	// network, MAC) masked in a session, by class and free bits k; Fixed
	// counts those the permutation left equal to themselves.
	Permuted, Fixed map[Kind]map[int]int
	// Same counts values of a substitution class left equal to themselves,
	// explicit rules aside. The expected count is zero.
	Same map[Kind]int
	// Unparsed counts values in the shape of their class that do not parse,
	// each left as a placeholder. The expected count is zero.
	Unparsed map[Kind]int
	// Ambiguous counts values an address class reads in two ways, such as
	// an IPv4 with leading zeros (decimal or octal); each leaves as a
	// placeholder. It is a rule, not a defect: no alarm.
	Ambiguous map[Kind]int
	// Alarms marks the classes whose counts are out of line: for a
	// permutation class E = Σ n_k·2^-k ≥ 10 and f > E + 3√E, for a
	// substitution class the first match.
	Alarms map[Kind]bool
}

type counters struct {
	mu              sync.Mutex
	seen            map[string]bool
	sessions        map[int]int
	values          map[[16]byte]bool
	permuted, fixed map[Kind]map[int]int
	same, unparsed  map[Kind]int
	ambiguous       map[Kind]int
	alarms          map[Kind]bool
	logged          bool
	logf            func(string, ...any)
}

func (c *counters) init() {
	if c.seen == nil {
		c.seen, c.sessions, c.values = make(map[string]bool), make(map[int]int), make(map[[16]byte]bool)
		c.permuted, c.fixed = make(map[Kind]map[int]int), make(map[Kind]map[int]int)
		c.same, c.unparsed, c.alarms = make(map[Kind]int), make(map[Kind]int), make(map[Kind]bool)
		c.ambiguous = make(map[Kind]int)
	}
}

func (c *counters) session(id string, version int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	if id != "" {
		if c.seen[id] {
			return
		}
		c.seen[id] = true
	}
	c.sessions[version]++
}

// permutation records one masked value of a permutation class with k free bits.
// digest is the value's keyed fingerprint in its session: history is masked
// again on every request, and a repeat is not a new observation. The alarm is
// blind until E >= 10, that is 10·2^k masked values of a class: about 655
// thousand at k = 16 and 168 million at k = 24; a broken permutation is not
// visible to it before that.
func (c *counters) permutation(kind Kind, k int, fixed bool, digest string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	var d [16]byte
	copy(d[:], digest)
	if c.values[d] {
		return
	}
	c.values[d] = true
	if c.permuted[kind] == nil {
		c.permuted[kind], c.fixed[kind] = make(map[int]int), make(map[int]int)
	}
	c.permuted[kind][k]++
	if fixed {
		c.fixed[kind][k]++
	}
	var e, f float64
	for k, n := range c.permuted[kind] {
		e += float64(n) * math.Exp2(-float64(k))
		f += float64(c.fixed[kind][k])
	}
	if e >= 10 && f > e+3*math.Sqrt(e) {
		c.alarm(kind)
	}
}

// substitution records one value of a substitution class; same reports that
// its pseudonym is the value itself.
func (c *counters) substitution(kind Kind, same bool) {
	if !same {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	c.same[kind]++
	c.alarm(kind)
}

// unparsedShape records one value in the shape of kind that does not parse;
// the first one alarms. An ambiguous value only counts.
func (c *counters) unparsedShape(kind Kind, ambiguous bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	if ambiguous {
		c.ambiguous[kind]++
		return
	}
	c.unparsed[kind]++
	c.alarm(kind)
}

// alarm marks kind and writes one log line per engine, without values.
func (c *counters) alarm(kind Kind) {
	c.alarms[kind] = true
	if c.logged {
		return
	}
	c.logged = true
	logf := c.logf
	if logf == nil {
		logf = log.Printf
	}
	logf("privacy: pseudonyms equal to originals out of line, class %s; see Engine.Counters", kind)
}

// Counters returns a copy of the running totals.
func (e *Engine) Counters() Counters {
	e.counts.mu.Lock()
	defer e.counts.mu.Unlock()
	out := Counters{Sessions: maps.Clone(e.counts.sessions), Permuted: map[Kind]map[int]int{}, Fixed: map[Kind]map[int]int{}, Same: maps.Clone(e.counts.same), Unparsed: maps.Clone(e.counts.unparsed), Ambiguous: maps.Clone(e.counts.ambiguous), Alarms: maps.Clone(e.counts.alarms)}
	for kind, n := range e.counts.permuted {
		out.Permuted[kind], out.Fixed[kind] = maps.Clone(n), maps.Clone(e.counts.fixed[kind])
	}
	return out
}
