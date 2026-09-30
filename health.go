package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"localrouter/internal/platform"
)

// Per-model stability. A model is scored by an exponential moving average of
// its success rate (alpha 0.25, so a failure costs about a quarter of the score
// and five clean calls in a row restore most of it) and by its time to first
// byte. After a failure the model is put in a cooldown that doubles with each
// consecutive failure, so a dead model is retried a little, not on every call.
// The state lives in models.json next to the binary and survives restarts.

const (
	healthAlpha    = 0.25
	cooldownBase   = 20 * time.Second
	cooldownMax    = 5 * time.Minute
	healthyMinimum = 0.5 // below this the preferred model loses its head start
)

type modelStat struct {
	OK          int       `json:"ok"`
	Fail        int       `json:"fail"`
	Consecutive int       `json:"consecutive"` // failures in a row
	Score       float64   `json:"score"`       // EWMA of success, 0..1
	TTFBMs      float64   `json:"ttfb_ms"`     // EWMA of time to first byte
	LastErr     string    `json:"last_err,omitempty"`
	LastAt      time.Time `json:"last_at"`
	LastOKAt    time.Time `json:"last_ok_at"`
	CoolUntil   time.Time `json:"cool_until"`
	Failovers   int       `json:"failovers"`    // times a request moved on from this model
	ServedAfter int       `json:"served_after"` // times this model rescued a request another one failed
	ProbeOK     int       `json:"probe_ok"`     // background checks; they shape the rating, not ok/fail
	ProbeFail   int       `json:"probe_fail"`
	ProbeAt     time.Time `json:"probe_at"`
}

func (m modelStat) Cooling() bool           { return time.Now().Before(m.CoolUntil) }
func (m modelStat) CoolLeft() time.Duration { return time.Until(m.CoolUntil).Truncate(time.Second) }
func (m modelStat) ScorePct() int           { return int(m.Score*100 + 0.5) }
func (m modelStat) TTFB() time.Duration     { return time.Duration(m.TTFBMs * float64(time.Millisecond)) }

type health struct {
	sessions map[string]sessionBinding
	mu       sync.Mutex
	m        map[string]*modelStat
	inflight map[string]int // requests being served right now, by model key
	path     string
	life     *lifecycle
	// balanceBy scores connections for new sessions of a balance pool; nil
	// means sessionsOnConnection.
	balanceBy balanceCriterion
}

func healthPath() string {
	if v, ok := os.LookupEnv("ROUTER_MODELS_STATE"); ok {
		return v
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "models.json")
	}
	return "models.json"
}

func newHealth(path string) *health {
	h := &health{m: map[string]*modelStat{}, inflight: map[string]int{}, path: path}
	if path == "" {
		return h
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("models state: %v", err)
		}
		return h
	}
	if err := json.Unmarshal(data, &h.m); err != nil {
		log.Printf("models state: %s: %v", path, err)
		h.m = map[string]*modelStat{}
	}
	return h
}

func (h *health) stat(model string) *modelStat {
	s, ok := h.m[model]
	if !ok {
		s = &modelStat{Score: 1} // unseen models are assumed fine, so they get tried
		h.m[model] = s
	}
	return s
}

// record notes one outcome; ttfb is only meaningful on success.
func (h *health) record(model string, ok bool, ttfb time.Duration, errMsg string) {
	h.note(model, ok, ttfb, errMsg, false)
}

// recordProbe notes a background check: it moves the rating, the latency and
// the cooldown like a real request, but is counted apart from real traffic.
func (h *health) recordProbe(model string, ok bool, ttfb time.Duration, errMsg string) {
	h.note(model, ok, ttfb, errMsg, true)
}

func (h *health) acquire(model string) {
	h.mu.Lock()
	h.inflight[model]++
	h.mu.Unlock()
}

func (h *health) release(model string) {
	h.mu.Lock()
	if h.inflight[model] > 0 {
		h.inflight[model]--
	}
	h.mu.Unlock()
}

func (h *health) note(model string, ok bool, ttfb time.Duration, errMsg string, probe bool) {
	h.mu.Lock()
	s := h.stat(model)
	now := time.Now()
	s.LastAt = now
	switch {
	case probe && ok:
		s.ProbeAt, s.ProbeOK = now, s.ProbeOK+1
	case probe:
		s.ProbeAt, s.ProbeFail = now, s.ProbeFail+1
	case ok:
		s.OK++
	default:
		s.Fail++
	}
	if ok {
		s.Consecutive = 0
		s.CoolUntil = time.Time{}
		s.LastOKAt = now
		s.LastErr = ""
		s.Score = s.Score*(1-healthAlpha) + healthAlpha
		ms := float64(ttfb) / float64(time.Millisecond)
		if s.TTFBMs == 0 {
			s.TTFBMs = ms
		} else {
			s.TTFBMs = s.TTFBMs*(1-healthAlpha) + ms*healthAlpha
		}
	} else {
		s.Consecutive++
		s.LastErr = errMsg
		s.Score = s.Score * (1 - healthAlpha)
		cool := cooldownBase << uint(min(s.Consecutive-1, 10))
		if cool > cooldownMax {
			cool = cooldownMax
		}
		s.CoolUntil = now.Add(cool)
	}
	h.mu.Unlock()
	h.save()
}

func (h *health) noteFailover(from, to string) {
	h.mu.Lock()
	h.stat(from).Failovers++
	h.stat(to).ServedAfter++
	h.mu.Unlock()
	h.save()
}

func (h *health) reset(model string) {
	h.mu.Lock()
	if model == "" {
		h.m = map[string]*modelStat{}
	} else {
		delete(h.m, model)
	}
	h.mu.Unlock()
	h.save()
}

func (h *health) save() {
	if h.path == "" || (h.life != nil && !h.life.writesSharedState()) {
		return
	}
	h.mu.Lock()
	data, err := json.MarshalIndent(h.m, "", "  ")
	h.mu.Unlock()
	if err != nil {
		return
	}
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("models state: %v", err)
		return
	}
	if err := platform.ReplaceFile(tmp, h.path); err != nil {
		log.Printf("models state: %v", err)
	}
}

// candidate is a configured model with its stats, in the order pick would try it.
type candidate struct {
	Key       string // provider/model, the id in stats and history
	Provider  provider
	Model     string
	Efforts   map[string]string
	Stat      modelStat
	Preferred bool
	InFlight  int // requests it is serving right now
}

// pick orders the configured local models for one request. Without failover
// only the preferred model is returned. With it, models in cooldown go last,
// the preferred model keeps its place at the head while it is healthy, and the
// rest are sorted by score, then by latency.
//
// With balance > 1 the head is chosen among the first balance healthy models
// (rating at least healthyMinimum, not cooling): the one with the fewest
// requests in flight goes first, ties keeping the rating order. The rest stay
// in rating order, so a request that fails on the chosen model moves to the
// best-rated one, and the load spreads off it again once it is busy.
//
// A pool route (any PoolType) ignores rating and balance and keeps pool order.
func (h *health) pick(c config) []candidate {
	l := c.Local
	var out []candidate
	for _, m := range l.Ordered() {
		p, _ := l.Provider(m.Provider)
		key := m.Key()
		out = append(out, candidate{Key: key, Provider: p, Model: m.Model, Efforts: m.Efforts, Stat: h.snapshot(key), Preferred: key == l.Preferred, InFlight: h.load(key)})
	}
	if !c.Failover && len(out) > 0 {
		return out[:1]
	}
	if c.PoolType != "" {
		// Pool order, not rating: members that are not cooling keep the order
		// the pool lists them in; cooling ones go last, soonest back first.
		sort.SliceStable(out, func(i, j int) bool {
			ci, cj := out[i].Stat.Cooling(), out[j].Stat.Cooling()
			if ci != cj {
				return cj
			}
			return ci && out[i].Stat.CoolUntil.Before(out[j].Stat.CoolUntil)
		})
		return out
	}
	rank := func(x candidate) int {
		switch {
		case x.Stat.Cooling():
			return 2
		case x.Preferred && x.Stat.Score >= healthyMinimum:
			return 0
		default:
			return 1
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		ra, rb := rank(a), rank(b)
		if ra != rb {
			return ra < rb
		}
		if ra == 2 {
			return a.Stat.CoolUntil.Before(b.Stat.CoolUntil)
		}
		if a.Stat.Score != b.Stat.Score {
			return a.Stat.Score > b.Stat.Score
		}
		return a.Stat.TTFBMs < b.Stat.TTFBMs
	})
	if c.Balance > 1 {
		group := 0
		for group < len(out) && group < c.Balance && rank(out[group]) < 2 && out[group].Stat.Score >= healthyMinimum {
			group++
		}
		best := 0
		for i := 1; i < group; i++ {
			if out[i].InFlight < out[best].InFlight {
				best = i
			}
		}
		if best > 0 {
			chosen := out[best]
			copy(out[1:best+1], out[:best])
			out[0] = chosen
		}
	}
	return out
}

func (h *health) load(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.inflight[key]
}

func (h *health) snapshot(key string) modelStat {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.m[key]; ok {
		return *s
	}
	return modelStat{Score: 1}
}
