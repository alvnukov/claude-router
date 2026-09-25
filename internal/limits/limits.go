// Package limits keeps the Anthropic subscription limits the router observes
// passively. Claude Code asks for them itself at
// api.anthropic.com/api/oauth/usage, past ANTHROPIC_BASE_URL, and the router
// must not repeat that call with the client's token. The only source is the
// anthropic-ratelimit-* headers on Anthropic's answers to the /v1/messages
// requests the client sent through the router; the proxy hook copies them and
// leaves the response alone. parseWindows turns them into per-window numbers;
// the unified names and scales it expects are the ones the Claude Code client
// reads, not yet confirmed against live traffic.
package limits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"localrouter/internal/platform"
)

const (
	// MaxAge is how long the view shows an observation.
	MaxAge = 30 * time.Minute
	// ClockSkew is how far in the future an observation may lie and still
	// count as current.
	ClockSkew = time.Minute

	prefix     = "anthropic-ratelimit-"
	maxHeaders = 64
	maxName    = 128
	maxValue   = 256
	maxLogged  = 16 // distinct header-name sets logged per process
	lockWait   = 2 * time.Second
)

// snapshot is one response's rate-limit headers: lower-case name to first
// value.
type snapshot struct {
	At      time.Time         `json:"at"`
	Headers map[string]string `json:"headers"`
}

// state is what limits.json holds: the newest response that carried the
// headers and the time of the newest one that did not.
type state struct {
	WithHeaders *snapshot `json:"with_headers,omitempty"`
	WithoutAt   time.Time `json:"without_at,omitzero"`
}

// Gate says whether this router may write the shared file: while two
// blue/green slots overlap, only the active one does.
type Gate interface {
	WritesSharedState() bool
}

// Store is the last observation of each kind, kept in memory and in the file
// New was given.
type Store struct {
	mu     sync.Mutex
	state  state
	maxAge time.Duration
	logged map[string]bool // name sets already logged

	path    string // "" keeps the snapshot in memory only
	gate    Gate   // nil: the only writer of path
	saveMu  sync.Mutex
	closed  bool
	dirty   chan struct{} // saver started on the first change
	quit    chan struct{}
	done    chan struct{}
	lastErr string // saver goroutine only
}

// View is what the settings page shows and GET /api/limits carries. Windows
// and Raw are set only while the snapshot is fresh, and then always, even
// empty.
type View struct {
	State         string            `json:"state"` // fresh | no_headers | unavailable
	ObservedAt    time.Time         `json:"observed_at,omitzero"`
	AgeSeconds    *int64            `json:"age_seconds,omitempty"`
	MaxAgeSeconds int64             `json:"max_age_seconds"`
	Windows       []Window          `json:"windows,omitzero"`
	Raw           map[string]string `json:"raw,omitzero"` // headers parseWindows did not read
}

// Window is one anthropic-ratelimit-<name>-<field> group. Only what the
// response carried is set: percentages from utilization, or from remaining
// and limit when utilization is absent.
type Window struct {
	Source           string    `json:"source,omitempty"`   // set in the /api/limits report
	Provider         string    `json:"provider,omitempty"` // Codex connection name
	Name             string    `json:"name"`
	RemainingPercent *float64  `json:"remaining_percent,omitempty"`
	UsedPercent      *float64  `json:"used_percent,omitempty"`
	Remaining        *int64    `json:"remaining,omitempty"`
	Limit            *int64    `json:"limit,omitempty"`
	ResetAt          time.Time `json:"reset_at,omitzero"`
	Status           string    `json:"status,omitempty"`
	WindowSeconds    int64     `json:"window_seconds,omitempty"`
	ObservedAt       time.Time `json:"observed_at,omitzero"` // set in the /api/limits report
	ResetIn          string    `json:"-"`
}

// Low marks a window the page highlights: a tenth or less left, or refused.
func (w Window) Low() bool {
	return w.RemainingPercent != nil && *w.RemainingPercent <= 10 || w.Status == "rejected"
}

func (v View) MaxAgeMinutes() int64 { return v.MaxAgeSeconds / 60 }

// New loads the last snapshot from path. A missing file is a fresh start; a
// broken one is reported and replaced by the next save.
func New(path string, maxAge time.Duration) *Store {
	l := &Store{maxAge: maxAge, logged: map[string]bool{}, path: path}
	if path == "" {
		return l
	}
	st, err := readState(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("anthropic limits: %s: %v", path, err)
		}
		return l
	}
	l.state = st
	return l
}

// SetGate makes saves wait for g; without a gate the store writes as the only
// writer. Call it before the store is used.
func (l *Store) SetGate(g Gate) { l.gate = g }

// readState reads limits.json and puts it through the same filter as a live
// response, so a hand-edited file cannot add other headers.
func readState(path string) (state, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return state{}, err
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return state{}, err
	}
	if st.WithHeaders != nil {
		h := http.Header{}
		for k, v := range st.WithHeaders.Headers {
			h[k] = []string{v}
		}
		st.WithHeaders.Headers = limitHeaders(h)
		if len(st.WithHeaders.Headers) == 0 || st.WithHeaders.At.IsZero() {
			st.WithHeaders = nil
		}
	}
	return st, nil
}

// replaces reports whether an observation at a should replace one at b as of
// now: it is newer, or b lies in the future and so cannot be trusted.
func replaces(a, b, now time.Time) bool {
	if a.IsZero() || a.After(now.Add(ClockSkew)) {
		return false
	}
	return b.IsZero() || a.After(b) || b.After(now.Add(ClockSkew))
}

func (s *snapshot) at() time.Time {
	if s == nil {
		return time.Time{}
	}
	return s.At
}

// merge keeps the newest trustworthy observation of each kind.
func (s state) merge(o state, now time.Time) state {
	if replaces(o.WithHeaders.at(), s.WithHeaders.at(), now) {
		s.WithHeaders = o.WithHeaders
	}
	if replaces(o.WithoutAt, s.WithoutAt, now) {
		s.WithoutAt = o.WithoutAt
	}
	return s
}

// limitHeaders copies the anthropic-ratelimit-* headers, bounded in count and
// size, and nothing else: no credentials or cookies can end up in a snapshot.
func limitHeaders(h http.Header) map[string]string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := map[string]string{}
	for _, k := range keys {
		name := strings.ToLower(k)
		if !strings.HasPrefix(name, prefix) || len(name) > maxName || len(h[k]) == 0 {
			continue
		}
		if _, dup := out[name]; dup {
			continue
		}
		if len(out) == maxHeaders {
			break
		}
		v := h[k][0]
		if len(v) > maxValue {
			v = v[:maxValue]
		}
		out[name] = v
	}
	return out
}

// Observe records one Anthropic response. Each distinct set of header names is
// logged once, names only, so the log shows what Anthropic actually sends.
// Saving happens in the background; the proxy never waits for the disk.
func (l *Store) Observe(h http.Header, at time.Time) {
	if l == nil {
		return
	}
	got := limitHeaders(h)
	names := sortedNames(got)
	key := strings.Join(names, ", ")
	l.mu.Lock()
	var next state
	if len(got) > 0 {
		next.WithHeaders = &snapshot{At: at, Headers: got}
	} else {
		next.WithoutAt = at
	}
	merged := l.state.merge(next, time.Now())
	if merged != l.state {
		l.state = merged
		l.schedule()
	}
	first := !l.logged[key] && len(l.logged) < maxLogged
	if first {
		l.logged[key] = true
	}
	l.mu.Unlock()
	if first {
		if key == "" {
			key = "none"
		}
		log.Printf("anthropic limits: response headers: %s", key)
	}
}

// schedule asks the saver for a write, starting it on first use. Pending
// requests collapse into one: the saver always writes the latest state.
// Called with l.mu held.
func (l *Store) schedule() {
	if l.path == "" || l.closed {
		return
	}
	if l.dirty == nil {
		l.dirty = make(chan struct{}, 1)
		l.quit = make(chan struct{})
		l.done = make(chan struct{})
		go l.saver()
	}
	select {
	case l.dirty <- struct{}{}:
	default:
	}
}

func (l *Store) saver() {
	defer close(l.done)
	for {
		select {
		case <-l.dirty:
			l.saveLogged()
		case <-l.quit:
			select {
			case <-l.dirty:
				l.saveLogged()
			default:
			}
			return
		}
	}
}

// saveLogged reports a failed save once per distinct error, without values.
func (l *Store) saveLogged() {
	msg := ""
	if err := l.Save(); err != nil {
		msg = err.Error()
	}
	if msg != "" && msg != l.lastErr {
		log.Printf("anthropic limits: save: %s", msg)
	}
	l.lastErr = msg
}

// Close writes a pending change and stops the saver. Later observations stay
// in memory.
func (l *Store) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	quit, done := l.quit, l.done
	l.mu.Unlock()
	if quit != nil {
		close(quit)
		<-done
	}
}

// Save merges with limits.json under a lock shared by every router process on
// the file, so a process with older state never overwrites a newer one, and
// takes over whatever newer state the file had.
func (l *Store) Save() error {
	if l == nil || l.path == "" || (l.gate != nil && !l.gate.WritesSharedState()) {
		return nil
	}
	l.saveMu.Lock()
	defer l.saveMu.Unlock()
	return withLock(l.path, func() error {
		now := time.Now()
		if disk, err := readState(l.path); err == nil {
			l.mu.Lock()
			l.state = l.state.merge(disk, now)
			l.mu.Unlock()
		}
		l.mu.Lock()
		st := l.state
		l.mu.Unlock()
		data, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			return err
		}
		if err := platform.MkdirPrivate(filepath.Dir(l.path)); err != nil {
			return err
		}
		return platform.WriteFileAtomic(l.path, append(data, '\n'), 0o600)
	})
}

// withLock runs fn holding the lock on path+".lock", shared by every router
// process on the file, waiting at most lockWait for another process to
// finish.
func withLock(path string, fn func() error) error {
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	defer cancel()
	err := platform.WithLock(ctx, path+".lock", fn)
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s.lock: held by another process", path)
	}
	return err
}

// ObserveResponse is the reverse proxy's ModifyResponse hook. It only reads
// the response and never fails it: an error returned here would replace the
// client's answer with a 502.
func (l *Store) ObserveResponse(resp *http.Response) error {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("anthropic limits: %v", p)
		}
	}()
	if resp.Request == nil || !strings.HasSuffix(resp.Request.URL.Path, "/v1/messages") {
		return nil
	}
	// An error without headers is not evidence that successful Anthropic
	// responses omit limits. Keep any headers the error actually carries.
	if (resp.StatusCode < 200 || resp.StatusCode >= 300) && len(limitHeaders(resp.Header)) == 0 {
		return nil
	}
	l.Observe(resp.Header, time.Now())
	return nil
}

// View shows the headers only while their response is at most maxAge old;
// after that the page says the data is unavailable instead of showing it.
func (l *Store) View(now time.Time) View {
	if l == nil {
		return View{State: "unavailable"}
	}
	l.mu.Lock()
	st := l.state
	l.mu.Unlock()
	v := View{State: "unavailable", MaxAgeSeconds: int64(l.maxAge / time.Second)}
	current := func(t time.Time) bool {
		age := now.Sub(t)
		return !t.IsZero() && age >= -ClockSkew && age <= l.maxAge
	}
	switch {
	case st.WithHeaders != nil && current(st.WithHeaders.At):
		v.State, v.ObservedAt = "fresh", st.WithHeaders.At
		v.Windows, v.Raw = parseWindows(st.WithHeaders.Headers)
		for i, w := range v.Windows {
			switch left := w.ResetAt.Sub(now); {
			case w.ResetAt.IsZero():
			case left <= 0:
				v.Windows[i].ResetIn = "ожидается обновление лимита"
			default:
				v.Windows[i].ResetIn = "через " + TimeLeft(left)
			}
		}
	case current(st.WithoutAt):
		v.State, v.ObservedAt = "no_headers", st.WithoutAt
	default:
		for _, t := range []time.Time{st.WithHeaders.at(), st.WithoutAt} {
			if replaces(t, v.ObservedAt, now) {
				v.ObservedAt = t
			}
		}
	}
	if !v.ObservedAt.IsZero() {
		age := max(int64(now.Sub(v.ObservedAt)/time.Second), 0)
		v.AgeSeconds = &age
	}
	return v
}

// TimeLeft says in Russian how long until a limit resets, to the minute.
func TimeLeft(d time.Duration) string {
	if d < time.Minute {
		return "менее минуты"
	}
	minutes := int(d.Minutes())
	if minutes < 60 {
		return fmt.Sprintf("%d мин.", minutes)
	}
	hours := minutes / 60
	if hours < 24 {
		return fmt.Sprintf("%d ч. %d мин.", hours, minutes%60)
	}
	return fmt.Sprintf("%d дн. %d ч.", hours/24, hours%24)
}

// parseWindows reads anthropic-ratelimit-<window>-<field> without knowing
// window names: the field is what follows the last hyphen. Utilization is a
// fraction of 1 and a numeric reset is unix seconds, as the unified headers
// the Claude Code client reads; remaining and limit are counts and a textual
// reset is RFC 3339, as the documented API headers. A header of another shape,
// or a value that does not parse, is returned in raw under its full name.
func parseWindows(h map[string]string) ([]Window, map[string]string) {
	found := map[string]*Window{}
	utilization := map[string]float64{}
	raw := map[string]string{}
	for name, value := range h {
		rest, prefixed := strings.CutPrefix(name, prefix)
		i := strings.LastIndexByte(rest, '-')
		if !prefixed || i <= 0 {
			raw[name] = value
			continue
		}
		window, field := rest[:i], rest[i+1:]
		w := found[window]
		if w == nil {
			w = &Window{Name: window}
		}
		ok := false
		switch field {
		case "utilization":
			var u float64
			if u, ok = parseFraction(value); ok {
				utilization[window] = u
			}
		case "remaining", "limit":
			n, err := strconv.ParseInt(value, 10, 64)
			if ok = err == nil && n >= 0; ok {
				if field == "remaining" {
					w.Remaining = &n
				} else {
					w.Limit = &n
				}
			}
		case "reset":
			var t time.Time
			if t, ok = parseReset(value); ok {
				w.ResetAt = t
			}
		case "status":
			if ok = value != ""; ok {
				w.Status = value
			}
		}
		if !ok {
			raw[name] = value
			continue
		}
		found[window] = w
	}

	windows := make([]Window, 0, len(found))
	for name, w := range found {
		if u, ok := utilization[name]; ok {
			used, left := round1(u*100), round1(min(max(100-u*100, 0), 100))
			w.UsedPercent, w.RemainingPercent = &used, &left
		} else if w.Remaining != nil && w.Limit != nil && *w.Limit > 0 {
			left := round1(min(float64(*w.Remaining)/float64(*w.Limit)*100, 100))
			w.RemainingPercent = &left
		}
		windows = append(windows, *w)
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].Name < windows[j].Name })
	return windows, raw
}

func parseFraction(s string) (float64, bool) {
	u, err := strconv.ParseFloat(s, 64)
	return u, err == nil && u >= 0 && !math.IsInf(u, 0) // NaN fails u >= 0
}

// parseReset takes unix seconds or RFC 3339. Milliseconds and other large
// numbers are not guessed at.
func parseReset(s string) (time.Time, bool) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n <= 0 || n >= 1e11 {
			return time.Time{}, false
		}
		return time.Unix(n, 0).UTC(), true
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

func sortedNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
