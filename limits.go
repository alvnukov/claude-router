package main

import (
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
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
)

// limitsSnapshot is one response's rate-limit headers: lower-case name to
// first value.
type limitsSnapshot struct {
	At      time.Time         `json:"at"`
	Headers map[string]string `json:"headers"`
}

type anthropicLimits struct {
	mu     sync.Mutex
	last   *limitsSnapshot // newest response that carried the headers
	maxAge time.Duration
	logged map[string]bool // name sets already logged
}

// anthropicLimitsView is what the settings page and GET /api/limits show.
// Header values are never part of it.
type anthropicLimitsView struct {
	State   string   `json:"state"` // unverified | unavailable
	Headers []string `json:"headers,omitempty"`
}

func newAnthropicLimits(path string, maxAge time.Duration) *anthropicLimits {
	return &anthropicLimits{maxAge: maxAge, logged: map[string]bool{}}
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
func (l *anthropicLimits) observe(h http.Header, at time.Time) {
	if l == nil {
		return
	}
	got := limitHeaders(h)
	names := sortedNames(got)
	key := strings.Join(names, ", ")
	l.mu.Lock()
	if len(got) > 0 && (l.last == nil || at.After(l.last.At)) {
		l.last = &limitsSnapshot{At: at, Headers: got}
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

func (l *anthropicLimits) view(now time.Time) anthropicLimitsView {
	if l == nil {
		return anthropicLimitsView{State: "unavailable"}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		return anthropicLimitsView{State: "unavailable"}
	}
	return anthropicLimitsView{State: "unverified", Headers: sortedNames(l.last.Headers)}
}

func sortedNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
