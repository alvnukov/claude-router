package privacy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	PreviewLimit       = 256 << 10
	previewOutputLimit = 1 << 20
	previewCapacity    = 4
	previewTTL         = 5 * time.Minute
)

// Lab is a bounded local preview workspace. No traffic, persistence, telemetry
// or execution is performed. Handles are capabilities; never log or list them.
type Lab struct {
	home  string
	ttl   time.Duration
	mu    sync.Mutex
	busy  chan struct{}
	items map[string]*previewItem
	stats LabState
}
type previewItem struct {
	engine  *Engine
	request *Request
	mode    string
	expires time.Time
	timer   *time.Timer
}
type PreviewInput struct {
	Mode    string          `json:"mode"`
	Input   string          `json:"input"`
	Rules   json.RawMessage `json:"rules"`
	Enabled bool            `json:"enabled"`
	Filter  string          `json:"filter"`
}
type PreviewResult struct {
	ID          string       `json:"id"`
	Output      string       `json:"output"`
	Expires     time.Time    `json:"expires"`
	Roundtrip   bool         `json:"roundtrip"`
	Enabled     bool         `json:"enabled"`
	Masked      map[Kind]int `json:"masked"`
	Unmasked    map[Kind]int `json:"unmasked"`
	Unexpected  int          `json:"unexpected"`
	InputBytes  int          `json:"inputBytes"`
	OutputBytes int          `json:"outputBytes"`
	DurationMS  float64      `json:"durationMs"`
}
type Observation struct {
	At           time.Time `json:"at"`
	Operation    string    `json:"operation"`
	Outcome      string    `json:"outcome"`
	DurationMS   float64   `json:"durationMs"`
	InputBytes   int       `json:"inputBytes"`
	OutputBytes  int       `json:"outputBytes"`
	Replacements int       `json:"replacements"`
}
type LabState struct {
	TrafficApplied bool          `json:"trafficApplied"`
	Started        time.Time     `json:"started"`
	Checks         int           `json:"checks"`
	Restores       int           `json:"restores"`
	Rejected       int           `json:"rejected"`
	Masked         map[Kind]int  `json:"masked"`
	Events         []Observation `json:"events"`
	Active         int           `json:"active"`
	InputLimit     int           `json:"inputLimit"`
	TTLSeconds     int           `json:"ttlSeconds"`
}

func NewLab(home string) *Lab {
	return newLab(home, previewTTL)
}
func newLab(home string, ttl time.Duration) *Lab {
	return &Lab{home: home, ttl: ttl, busy: make(chan struct{}, 1), items: map[string]*previewItem{}, stats: LabState{Started: time.Now(), Masked: map[Kind]int{}, Events: []Observation{}, InputLimit: PreviewLimit, TTLSeconds: int(ttl.Seconds())}}
}
func (l *Lab) State() LabState {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.stats
	s.Masked, s.Events, s.Active = maps.Clone(s.Masked), slices.Clone(s.Events), len(l.items)
	return s
}
func (l *Lab) enter(ctx context.Context) error {
	if ctx.Err() != nil {
		return errors.New("cancelled")
	}
	select {
	case l.busy <- struct{}{}:
		return nil
	default:
		return errors.New("busy")
	}
}
func (l *Lab) observe(op string, start time.Time, input int, r PreviewResult, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	o := Observation{At: time.Now(), Operation: op, Outcome: "ok", InputBytes: input, OutputBytes: r.OutputBytes, DurationMS: float64(time.Since(start).Microseconds()) / 1000}
	if op == "mask" {
		l.stats.Checks++
	} else {
		l.stats.Restores++
	}
	if err != nil {
		l.stats.Rejected++
		o.Outcome = "rejected"
	} else if !r.Enabled {
		o.Outcome = "bypass"
	} else if r.Unexpected > 0 {
		o.Outcome = "needs_correction"
	}
	for kind, count := range r.Masked {
		l.stats.Masked[kind] += count
		o.Replacements += count
	}
	for _, count := range r.Unmasked {
		o.Replacements += count
	}
	l.stats.Events = append([]Observation{o}, l.stats.Events...)
	if len(l.stats.Events) > 20 {
		l.stats.Events = l.stats.Events[:20]
	}
}
func previewBody(mode, input string, limit int) ([]byte, error) {
	if len(input) > limit {
		return nil, errors.New("too_large")
	}
	if !utf8.ValidString(input) {
		return nil, errors.New("invalid_input")
	}
	if mode == "text" {
		return json.Marshal(struct {
			System string `json:"system"`
		}{input})
	}
	if mode != "json" {
		return nil, errors.New("invalid_mode")
	}
	root, err := scanJSON([]byte(input))
	if err != nil || root.kind != '{' || root.get("messages") == nil && root.get("system") == nil && root.get("content") == nil {
		return nil, errors.New("invalid_input")
	}
	return []byte(input), nil
}
func previewText(mode string, body []byte) string {
	if mode == "text" {
		s, _ := lookupString(body, "system")
		return s
	}
	return string(body)
}

func (l *Lab) Preview(ctx context.Context, in PreviewInput) (result PreviewResult, err error) {
	if err = l.enter(ctx); err != nil {
		return result, err
	}
	defer func() { <-l.busy }()
	start := time.Now()
	defer func() { l.observe("mask", start, len(in.Input), result, err) }()
	body, err := previewBody(in.Mode, in.Input, PreviewLimit)
	if err != nil {
		return result, err
	}
	if len(in.Rules) > 64<<10 {
		return result, errors.New("invalid_rules")
	}
	rules, err := ParseRules(in.Rules)
	if err != nil {
		return result, errors.New("invalid_rules")
	}
	if in.Filter != "" {
		if !slices.Contains(FilterIDs(), in.Filter) {
			return result, errors.New("invalid_filter")
		}
		rules.Filters = map[string]bool{}
		for _, id := range FilterIDs() {
			rules.Filters[id] = id == in.Filter
		}
		in.Enabled = true // explicit isolated test, independent of profile switches
	}
	l.mu.Lock()
	full := len(l.items) >= previewCapacity
	l.mu.Unlock()
	if full {
		return result, errors.New("capacity")
	}
	item := &previewItem{mode: in.Mode}
	masked, restored := body, body
	if in.Enabled {
		item.engine, err = Open("", rules, Options{ephemeral: true})
		if err != nil {
			return result, errors.New("invalid_rules")
		}
		masked, item.request, err = item.engine.Mask(body)
		if err != nil {
			return result, errors.New("mask_rejected")
		}
		defer func() {
			if err != nil {
				item.request.Close()
			}
		}()
		result.Masked = item.request.Stats().Masked
		restored, err = item.engine.UnmaskJSON(item.request, masked)
		if err != nil {
			return PreviewResult{}, errors.New("roundtrip_rejected")
		}
	}
	result.Output = previewText(in.Mode, masked)
	if len(result.Output) > previewOutputLimit {
		return PreviewResult{}, errors.New("too_large")
	}
	if ctx.Err() != nil {
		return PreviewResult{}, errors.New("cancelled")
	}
	id := make([]byte, 24)
	if _, err = rand.Read(id); err != nil {
		return PreviewResult{}, errors.New("unavailable")
	}
	item.expires = time.Now().Add(l.ttl)
	result.ID, result.Expires = hex.EncodeToString(id), item.expires
	result.Enabled, result.Roundtrip = in.Enabled, bytes.Equal(restored, body)
	result.InputBytes, result.OutputBytes = len(in.Input), len(result.Output)
	result.DurationMS = float64(time.Since(start).Microseconds()) / 1000
	if result.Masked == nil {
		result.Masked = map[Kind]int{}
	}
	result.Unmasked = map[Kind]int{}
	l.mu.Lock()
	l.items[result.ID] = item
	// Capture just the ID, never the result (which contains text).
	handle := result.ID
	item.timer = time.AfterFunc(l.ttl, func() { l.Clear(handle) })
	l.mu.Unlock()
	return result, nil
}
func (l *Lab) Restore(ctx context.Context, id, input string) (result PreviewResult, err error) {
	if err = l.enter(ctx); err != nil {
		return result, err
	}
	defer func() { <-l.busy }()
	start := time.Now()
	defer func() { l.observe("restore", start, len(input), result, err) }()
	l.mu.Lock()
	item := l.items[id]
	l.mu.Unlock()
	if item == nil || !time.Now().Before(item.expires) {
		return result, errors.New("expired")
	}
	body, err := previewBody(item.mode, input, previewOutputLimit)
	if err != nil {
		return result, err
	}
	out := body
	result.Unmasked, result.Masked = map[Kind]int{}, map[Kind]int{}
	if item.request != nil {
		before := item.request.Stats()
		out, err = item.engine.UnmaskJSON(item.request, body)
		if err != nil {
			return PreviewResult{}, errors.New("restore_rejected")
		}
		after := item.request.Stats()
		for kind, count := range after.Unmasked {
			result.Unmasked[kind] = count - before.Unmasked[kind]
		}
		result.Unexpected = after.Unexpected - before.Unexpected
	}
	if ctx.Err() != nil {
		return PreviewResult{}, errors.New("cancelled")
	}
	result.Output, result.Enabled = previewText(item.mode, out), item.request != nil
	if len(result.Output) > previewOutputLimit {
		return PreviewResult{}, errors.New("too_large")
	}
	result.InputBytes, result.OutputBytes = len(input), len(result.Output)
	result.DurationMS = float64(time.Since(start).Microseconds()) / 1000
	return result, nil
}
func (l *Lab) Clear(id string) {
	l.mu.Lock()
	item := l.items[id]
	delete(l.items, id)
	l.mu.Unlock()
	if item != nil {
		item.timer.Stop()
		if item.request != nil {
			item.request.Close()
		}
	}
}
