package privacy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"sync"

	"localrouter/internal/platform"
)

const TrafficInputLimit = 32 << 20
const TrafficOutputLimit = 16 << 20

var errTraffic = errors.New("privacy: request rejected; check the profile and supported format")
var errRestore = errors.New("privacy: response rejected; copy exact pseudonyms from the current context and retry without changing or encoding them")

type Runtime struct {
	home           string
	mu             sync.Mutex
	revision       string
	policy         *Policy
	clientControls clientControlTable
	stats          RuntimeState
	legacyCleaned  bool
}
type RuntimeState struct {
	Status          string       `json:"status"`
	Enabled         bool         `json:"enabled"`
	Protected       int          `json:"protected"`
	Detected        int          `json:"detected"`
	DetectionErrors int          `json:"detection_errors,omitempty"`
	Findings        map[Kind]int `json:"findings"`
	Bypassed        int          `json:"bypassed"`
	Rejected        int          `json:"rejected"`
	Restored        int          `json:"restored"`
	Active          int          `json:"active"`
	Buffered        bool         `json:"buffered"`
	LegacyRemoved   int          `json:"legacy_removed,omitempty"`
	LegacyUnknown   int          `json:"legacy_unknown,omitempty"`
}
type Policy struct {
	config         *Profiles
	engines        map[string]*Engine
	clientControls clientControlTable
	runtime        *Runtime
}
type Exchange struct {
	restoreMu sync.Mutex
	engine    *Engine
	request   *Request
	runtime   *Runtime
	once      sync.Once
}

func NewRuntime(home string) *Runtime {
	return &Runtime{home: home, stats: RuntimeState{Status: "missing", Buffered: true, Findings: map[Kind]int{}}}
}
func (r *Runtime) Snapshot() (*Policy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot := NewLab(r.home).Config()
	if snapshot.Status == "missing" && r.home != "" {
		if _, err := os.Lstat(filepath.Join(r.home, "privacy-required")); !errors.Is(err, os.ErrNotExist) {
			snapshot.Status = "invalid"
		}
	}
	r.stats.Status = snapshot.Status
	if snapshot.Status == "invalid" || snapshot.Status == "unavailable" {
		r.stats.Enabled = false
		return nil, errTraffic
	}
	if r.policy != nil && r.revision == snapshot.Revision {
		r.stats.Enabled = r.policy.Enabled()
		r.stats.Buffered = !r.policy.observationOnly()
		return r.policy, nil
	}
	if !r.legacyCleaned && r.home != "" {
		// Once per process and before any engine opens: old session files
		// hold real values. A failure leaves them as they were and is tried
		// again with the next policy; traffic does not depend on it.
		removed, unknown, err := cleanLegacy(filepath.Join(r.home, "privacy-runtime"))
		r.stats.LegacyRemoved += removed
		r.stats.LegacyUnknown = unknown
		r.legacyCleaned = err == nil
	}
	p := &Policy{config: snapshot.Config, engines: map[string]*Engine{}, clientControls: r.clientControls.clone(), runtime: r}
	if p.Enabled() {
		// Marker survives restart: deleting the policy is never an off switch.
		if err := platform.WritePrivateAtomic(filepath.Join(r.home, "privacy-required"), []byte("privacy profiles required\n")); err != nil {
			r.stats.Status = "invalid"
			r.stats.Enabled = false
			return nil, errTraffic
		}
		for _, profile := range p.config.Profiles {
			if !profile.Enabled {
				continue
			}
			rules, err := ParseRules(profile.Rules)
			if err != nil {
				return nil, errTraffic
			}
			sum := sha256.Sum256(append([]byte("transport-v1\x00"), profile.Rules...))
			namespace := filepath.Join(r.home, "privacy-runtime", profile.ID+"-"+hex.EncodeToString(sum[:]))
			opt := Options{supportedOnly: true}
			if effectiveMode(profile.Mode) == ModeDetect {
				namespace = ""
				opt.ephemeral = true
			}
			engine, err := Open(namespace, rules, opt)
			if err != nil {
				r.stats.Status = "invalid"
				r.stats.Enabled = false
				return nil, errTraffic
			}
			p.engines[profile.ID] = engine
		}
	}
	r.revision, r.policy = snapshot.Revision, p
	r.stats.Enabled = p.Enabled()
	r.stats.Buffered = !p.observationOnly()
	return p, nil
}
func (p *Policy) Enabled() bool { return p != nil && p.config != nil && p.config.Enabled }
func (r *Runtime) State() RuntimeState {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.stats
	state.Findings = maps.Clone(state.Findings)
	return state
}
func (r *Runtime) Reject() { r.mu.Lock(); r.stats.Rejected++; r.mu.Unlock() }
func (p *Policy) Prepare(target Target, body []byte) (*Exchange, []byte, error) {
	resolution := p.config.Resolve(target)
	if !resolution.Enabled {
		p.runtime.mu.Lock()
		p.runtime.stats.Bypassed++
		p.runtime.mu.Unlock()
		return nil, body, nil
	}
	e := p.engines[resolution.Profile]
	if resolution.Mode == ModeDetect {
		var counts map[Kind]int
		err := errTraffic
		if e != nil {
			counts, err = e.Detect(body)
		}
		p.runtime.mu.Lock()
		if err != nil {
			p.runtime.stats.DetectionErrors++
		} else {
			p.runtime.stats.Detected++
			for kind, count := range counts {
				p.runtime.stats.Findings[kind] += count
			}
		}
		p.runtime.mu.Unlock()
		return nil, body, nil
	}
	if e == nil || len(body) > TrafficInputLimit {
		return nil, nil, errTraffic
	}
	if err := e.checkTransport(body); err != nil {
		return nil, nil, errTraffic
	}
	masked, req, err := e.Mask(body)
	if err != nil {
		return nil, nil, errTraffic
	}
	p.runtime.mu.Lock()
	p.runtime.stats.Protected++
	p.runtime.stats.Active++
	p.runtime.mu.Unlock()
	return &Exchange{engine: e, request: req, runtime: p.runtime}, masked, nil
}
func (x *Exchange) Close() {
	if x == nil {
		return
	}
	x.once.Do(func() { x.request.Close(); x.runtime.mu.Lock(); x.runtime.stats.Active--; x.runtime.mu.Unlock() })
}
func (x *Exchange) Restore(body []byte, stream bool) ([]byte, error) {
	x.restoreMu.Lock()
	defer x.restoreMu.Unlock()
	if len(body) > TrafficOutputLimit {
		return nil, errRestore
	}
	x.request.mu.Lock()
	x.request.budget = &restoreBudget{remaining: TrafficOutputLimit - len(body), stream: stream}
	x.request.mu.Unlock()
	defer func() { x.request.mu.Lock(); x.request.budget = nil; x.request.mu.Unlock() }()
	var out []byte
	var err error
	if stream {
		if err = x.checkTrafficSSE(body); err != nil {
			return nil, errRestore
		}
		var buf trafficBuffer
		w := x.engine.NewStreamUnmasker(x.request, &buf)
		_, err = w.Write(body)
		closeErr := w.Close()
		if err == nil {
			err = closeErr
		}
		out = buf.Bytes()
	} else {
		if err = x.checkResponse(body); err == nil {
			out, err = x.engine.UnmaskJSON(x.request, body)
		}
	}
	if err != nil || !x.engine.opt.supportedOnly && x.request.Stats().Unexpected > 0 || len(out) > TrafficOutputLimit {
		return nil, errRestore
	}
	x.runtime.mu.Lock()
	x.runtime.stats.Restored++
	x.runtime.mu.Unlock()
	return out, nil
}
