package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Anthropic subscription limits are observed passively. Claude Code asks for
// them itself at api.anthropic.com/api/oauth/usage, past ANTHROPIC_BASE_URL,
// and the router must not repeat that call with the client's token. The only
// source is the anthropic-ratelimit-* headers on Anthropic's answers to the
// /v1/messages requests the client sent through the router; the proxy hook
// copies them and leaves the response alone. What these headers mean for the
// subscription is not confirmed yet, so only their names are shown.

const (
	anthropicLimitsPrefix = "anthropic-ratelimit-"
	anthropicLimitsMaxAge = 30 * time.Minute
	limitsMaxHeaders      = 64
	limitsMaxName         = 128
	limitsMaxValue        = 256
	limitsMaxLogged       = 16 // distinct header-name sets logged per process
	limitsClockSkew       = time.Minute
	limitsLockWait        = 2 * time.Second
)

// limitsSnapshot is one response's rate-limit headers: lower-case name to
// first value.
type limitsSnapshot struct {
	At      time.Time         `json:"at"`
	Headers map[string]string `json:"headers"`
}

// limitsState is what limits.json holds: the newest response that carried the
// headers and the time of the newest one that did not.
type limitsState struct {
	WithHeaders *limitsSnapshot `json:"with_headers,omitempty"`
	WithoutAt   time.Time       `json:"without_at,omitzero"`
}

type anthropicLimits struct {
	mu     sync.Mutex
	state  limitsState
	maxAge time.Duration
	logged map[string]bool // name sets already logged

	path    string // "" keeps the snapshot in memory only
	saveMu  sync.Mutex
	closed  bool
	dirty   chan struct{} // saver started on the first change
	quit    chan struct{}
	done    chan struct{}
	lastErr string // saver goroutine only
}

// anthropicLimitsView is what the settings page and GET /api/limits show.
// Header values are never part of it.
type anthropicLimitsView struct {
	State         string    `json:"state"` // unverified | no_headers | unavailable
	ObservedAt    time.Time `json:"observed_at,omitzero"`
	AgeSeconds    *int64    `json:"age_seconds,omitempty"`
	MaxAgeSeconds int64     `json:"max_age_seconds"`
	Headers       []string  `json:"headers,omitempty"`
}

func (v anthropicLimitsView) MaxAgeMinutes() int64 { return v.MaxAgeSeconds / 60 }

func limitsPath() string {
	if v, ok := os.LookupEnv("ROUTER_ANTHROPIC_LIMITS_FILE"); ok {
		return v // "" disables
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "limits.json")
	}
	return "limits.json"
}

// newAnthropicLimits loads the last snapshot from path. A missing file is a
// fresh start; a broken one is reported and replaced by the next save.
func newAnthropicLimits(path string, maxAge time.Duration) *anthropicLimits {
	l := &anthropicLimits{maxAge: maxAge, logged: map[string]bool{}, path: path}
	if path == "" {
		return l
	}
	st, err := readLimitsState(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("anthropic limits: %s: %v", path, err)
		}
		return l
	}
	l.state = st
	return l
}

// readLimitsState reads limits.json and puts it through the same filter as a
// live response, so a hand-edited file cannot add other headers.
func readLimitsState(path string) (limitsState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return limitsState{}, err
	}
	var st limitsState
	if err := json.Unmarshal(data, &st); err != nil {
		return limitsState{}, err
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
	if a.IsZero() || a.After(now.Add(limitsClockSkew)) {
		return false
	}
	return b.IsZero() || a.After(b) || b.After(now.Add(limitsClockSkew))
}

func (s *limitsSnapshot) at() time.Time {
	if s == nil {
		return time.Time{}
	}
	return s.At
}

// merge keeps the newest trustworthy observation of each kind.
func (s limitsState) merge(o limitsState, now time.Time) limitsState {
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
		if !strings.HasPrefix(name, anthropicLimitsPrefix) || len(name) > limitsMaxName || len(h[k]) == 0 {
			continue
		}
		if _, dup := out[name]; dup {
			continue
		}
		if len(out) == limitsMaxHeaders {
			break
		}
		v := h[k][0]
		if len(v) > limitsMaxValue {
			v = v[:limitsMaxValue]
		}
		out[name] = v
	}
	return out
}

// observe records one Anthropic response. Each distinct set of header names is
// logged once, names only, so the log shows what Anthropic actually sends.
// Saving happens in the background; the proxy never waits for the disk.
func (l *anthropicLimits) observe(h http.Header, at time.Time) {
	if l == nil {
		return
	}
	got := limitHeaders(h)
	names := sortedNames(got)
	key := strings.Join(names, ", ")
	l.mu.Lock()
	var next limitsState
	if len(got) > 0 {
		next.WithHeaders = &limitsSnapshot{At: at, Headers: got}
	} else {
		next.WithoutAt = at
	}
	merged := l.state.merge(next, at)
	if merged != l.state {
		l.state = merged
		l.schedule()
	}
	first := !l.logged[key] && len(l.logged) < limitsMaxLogged
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
func (l *anthropicLimits) schedule() {
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

func (l *anthropicLimits) saver() {
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
func (l *anthropicLimits) saveLogged() {
	msg := ""
	if err := l.save(); err != nil {
		msg = err.Error()
	}
	if msg != "" && msg != l.lastErr {
		log.Printf("anthropic limits: save: %s", msg)
	}
	l.lastErr = msg
}

// close writes a pending change and stops the saver. Later observations stay
// in memory.
func (l *anthropicLimits) close() {
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

// save merges with limits.json under a lock shared by every router process on
// the file, so a process with older state never overwrites a newer one, and
// takes over whatever newer state the file had.
func (l *anthropicLimits) save() error {
	if l == nil || l.path == "" {
		return nil
	}
	l.saveMu.Lock()
	defer l.saveMu.Unlock()
	return withLimitsLock(l.path, func() error {
		now := time.Now()
		if disk, err := readLimitsState(l.path); err == nil {
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
		return writePrivateAtomic(l.path, append(data, '\n'))
	})
}

// withLimitsLock runs fn holding an exclusive flock on path+".lock", waiting
// at most limitsLockWait for another process to finish.
func withLimitsLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	deadline := time.Now().Add(limitsLockWait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			return fn()
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s.lock: held by another process", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// observeResponse is the reverse proxy's ModifyResponse hook. It only reads the
// response and never fails it: an error returned here would replace the
// client's answer with a 502.
func (l *anthropicLimits) observeResponse(resp *http.Response) error {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("anthropic limits: %v", p)
		}
	}()
	if resp.Request == nil || !strings.HasSuffix(resp.Request.URL.Path, "/v1/messages") {
		return nil
	}
	l.observe(resp.Header, time.Now())
	return nil
}

// view shows the headers only while their response is at most maxAge old;
// after that the page says the data is unavailable instead of showing it.
func (l *anthropicLimits) view(now time.Time) anthropicLimitsView {
	if l == nil {
		return anthropicLimitsView{State: "unavailable"}
	}
	l.mu.Lock()
	st := l.state
	l.mu.Unlock()
	v := anthropicLimitsView{State: "unavailable", MaxAgeSeconds: int64(l.maxAge / time.Second)}
	current := func(t time.Time) bool {
		age := now.Sub(t)
		return !t.IsZero() && age >= -limitsClockSkew && age <= l.maxAge
	}
	switch {
	case st.WithHeaders != nil && current(st.WithHeaders.At):
		v.State, v.ObservedAt = "unverified", st.WithHeaders.At
		v.Headers = sortedNames(st.WithHeaders.Headers)
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

func sortedNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
