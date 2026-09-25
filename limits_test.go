package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"localrouter/internal/history"
	"localrouter/internal/platform"
)

// The unified-* names below are the ones the Claude Code client reads; every
// value is synthetic. Live traffic has not confirmed either, so nothing below
// claims what Anthropic sends.

func TestAnthropicLimitsKeepsOnlyRatelimitHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Requests-Remaining", "41")
	h["anthropic-ratelimit-example-status"] = []string{"allowed", "second"}
	h.Set("Authorization", "Bearer secret-token")
	h.Set("X-Api-Key", "secret-key")
	h.Set("Set-Cookie", "session=secret")
	h.Set("Request-Id", "req_1")
	h.Set("Retry-After", "5")
	h.Set("Anthropic-Ratelimit-Long", strings.Repeat("x", 1000))

	got := limitHeaders(h)
	want := map[string]string{
		"anthropic-ratelimit-requests-remaining": "41",
		"anthropic-ratelimit-example-status":     "allowed",
		"anthropic-ratelimit-long":               strings.Repeat("x", limitsMaxValue),
	}
	if len(got) != len(want) {
		t.Fatalf("kept %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q", k, got[k], v)
		}
	}

	many := http.Header{}
	for i := 0; i < 3*limitsMaxHeaders; i++ {
		many.Set(fmt.Sprintf("Anthropic-Ratelimit-N%03d", i), "1")
	}
	kept := limitHeaders(many)
	if len(kept) != limitsMaxHeaders || kept["anthropic-ratelimit-n000"] == "" || kept[fmt.Sprintf("anthropic-ratelimit-n%03d", limitsMaxHeaders)] != "" {
		t.Fatalf("cap is not the first %d names in order: %d kept", limitsMaxHeaders, len(kept))
	}
	long := http.Header{}
	long.Set("Anthropic-Ratelimit-"+strings.Repeat("n", 500), "1")
	if n := len(limitHeaders(long)); n != 0 {
		t.Fatal("unbounded header name kept")
	}
	if n := len(limitHeaders(nil)); n != 0 {
		t.Fatalf("nil header kept %d", n)
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return &buf
}

func TestAnthropicLimitsLogsNamesOnceWithoutValues(t *testing.T) {
	buf := captureLog(t)
	l := newAnthropicLimits("", anthropicLimitsMaxAge)
	now := time.Now()
	l.observe(http.Header{}, now)
	l.observe(http.Header{}, now.Add(time.Second))
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-B", "value-b-7")
	h.Set("Anthropic-Ratelimit-A", "value-a-9")
	l.observe(h, now.Add(2*time.Second))
	l.observe(h, now.Add(3*time.Second))

	out := buf.String()
	if strings.Count(out, "\n") != 2 {
		t.Fatalf("want one line per distinct name set, got:\n%s", out)
	}
	if !strings.Contains(out, "anthropic limits: response headers: none") {
		t.Fatalf("empty set not logged:\n%s", out)
	}
	if !strings.Contains(out, "anthropic limits: response headers: anthropic-ratelimit-a, anthropic-ratelimit-b") {
		t.Fatalf("names not logged sorted:\n%s", out)
	}
	if strings.Contains(out, "value-") {
		t.Fatalf("header values leaked into the log:\n%s", out)
	}
}

func TestAnthropicLimitsHookNeverFails(t *testing.T) {
	captureLog(t)
	var nilStore *anthropicLimits
	if err := nilStore.observeResponse(&http.Response{Header: http.Header{}}); err != nil {
		t.Fatal(err)
	}
	l := newAnthropicLimits("", anthropicLimitsMaxAge)
	if err := l.observeResponse(nil); err != nil {
		t.Fatal(err)
	}
	if err := l.observeResponse(&http.Response{Header: http.Header{}}); err != nil {
		t.Fatal(err)
	}
}

func TestAnthropicLimitsErrorWithoutHeadersDoesNotClaimAbsence(t *testing.T) {
	buf := captureLog(t)
	l := newAnthropicLimits("", anthropicLimitsMaxAge)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	for _, status := range []int{http.StatusBadGateway, 529} {
		resp := &http.Response{StatusCode: status, Request: req, Header: http.Header{}}
		if err := l.observeResponse(resp); err != nil {
			t.Fatal(err)
		}
		if v := l.view(time.Now()); v.State != "unavailable" {
			t.Fatalf("%d without headers claimed Anthropic sent no limits: %+v", status, v)
		}
	}
	if strings.Contains(buf.String(), "response headers: none") {
		t.Fatalf("error response logged as evidence of absent headers: %s", buf.String())
	}
	if err := l.observeResponse(&http.Response{StatusCode: http.StatusNoContent, Request: req, Header: http.Header{}}); err != nil {
		t.Fatal(err)
	}
	if v := l.view(time.Now()); v.State != "no_headers" {
		t.Fatalf("successful response without headers must be diagnostic: %+v", v)
	}
}

// countingTransport counts every outbound request the process makes through
// the default transport, which the reverse proxy uses.
type countingTransport struct {
	next http.RoundTripper
	n    atomic.Int64
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.next.RoundTrip(r)
}

func countOutbound(t *testing.T) *countingTransport {
	t.Helper()
	prev := http.DefaultTransport
	ct := &countingTransport{next: prev}
	http.DefaultTransport = ct
	t.Cleanup(func() { http.DefaultTransport = prev })
	return ct
}

// limitsRouter is the API handler with sonnet sent to Anthropic at upstream
// and opus/high to the local provider at local.
func limitsRouter(t *testing.T, upstream, local string) (*uiServer, http.Handler) {
	t.Helper()
	return limitsRouterStore(t, upstream, local, history.New(10, ""))
}

func limitsRouterStore(t *testing.T, upstream, local string, st *history.Store) (*uiServer, http.Handler) {
	t.Helper()
	cs, h, _ := profileFixture(t)
	up, _ := url.Parse(upstream)
	cs.c.upstream = up
	l := cs.get().local.clone()
	l.FamilyRoutes["sonnet"] = map[string]modelRoute{"default": {Mode: "anthropic"}}
	if local != "" {
		l.Providers[0].BaseURL = local
	}
	if err := cs.applyLocal(l, true); err != nil {
		t.Fatal(err)
	}
	u := newUIServer(st, cs, h)
	return u, newMainHandler(cs.get(), cs, st, h, u)
}

// A streamed Anthropic response reaches the client unchanged and unbuffered,
// the router makes exactly the one request the client asked for, and the
// response headers land in the snapshot.
func TestAnthropicLimitsProxyIsTransparent(t *testing.T) {
	captureLog(t)
	release := make(chan struct{})
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Anthropic-Ratelimit-Example-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "41")
		w.Header().Set("Request-Id", "req_upstream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		_, _ = io.WriteString(w, "event: message_stop\ndata: {}\n\n")
	}))
	defer upstream.Close()
	out := countOutbound(t)

	u, handler := limitsRouter(t, upstream.URL, "")
	router := httptest.NewServer(handler)
	defer router.Close()

	body := `{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", router.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-token")
	resp, err := (&http.Client{Transport: &http.Transport{}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for k, v := range map[string]string{
		"Content-Type":                           "text/event-stream",
		"Anthropic-Ratelimit-Example-Status":     "allowed",
		"Anthropic-Ratelimit-Requests-Remaining": "41",
		"Request-Id":                             "req_upstream",
	} {
		if got := resp.Header.Values(k); len(got) != 1 || got[0] != v {
			t.Fatalf("client header %s = %v, want %q", k, got, v)
		}
	}
	rd := bufio.NewReader(resp.Body)
	first, err := rd.ReadString('\n')
	if err != nil || first != "event: message_start\n" {
		t.Fatalf("first event before upstream finished: %q %v", first, err)
	}
	close(release)
	rest, _ := io.ReadAll(rd)
	if got := first + string(rest); got != "event: message_start\ndata: {}\n\nevent: message_stop\ndata: {}\n\n" {
		t.Fatalf("body changed: %q", got)
	}
	if n := upstreamCalls.Load(); n != 1 {
		t.Fatalf("upstream saw %d requests", n)
	}
	if n := out.n.Load(); n != 1 {
		t.Fatalf("router made %d outbound requests, want 1", n)
	}
	v := u.limits.view(time.Now())
	if v.State != "fresh" || limitKeys(v) != "example,requests" || v.Windows[0].Status != "allowed" || v.Windows[1].Remaining == nil || *v.Windows[1].Remaining != 41 {
		t.Fatalf("snapshot %+v", v)
	}
}

// limitKeys is a view's parsed window names and raw header names, sorted:
// what the tests below compare to tell snapshots apart.
func limitKeys(v anthropicLimitsView) string {
	var keys []string
	for _, w := range v.Windows {
		keys = append(keys, w.Name)
	}
	for k := range v.Raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// Only Anthropic's answers to /v1/messages count: a local route never reaches
// the proxy, and other proxied paths say nothing about the subscription.
func TestAnthropicLimitsIgnoresLocalAndOtherPaths(t *testing.T) {
	captureLog(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Example-Status", "allowed")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"input_tokens":1}`)
	}))
	defer upstream.Close()
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Example-Status", "local")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer local.Close()

	u, handler := limitsRouter(t, upstream.URL, local.URL)
	send := func(path, body string) int {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
		return w.Code
	}
	if code := send("/v1/messages", `{"model":"claude-opus-5","output_config":{"effort":"high"},"messages":[{"role":"user","content":"hi"}]}`); code != http.StatusServiceUnavailable {
		t.Fatalf("local route: %d", code)
	}
	if code := send("/v1/messages/count_tokens", `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`); code != http.StatusOK {
		t.Fatalf("count_tokens: %d", code)
	}
	if code := send("/v1/models", ""); code != http.StatusOK {
		t.Fatalf("catch-all: %d", code)
	}
	if v := u.limits.view(time.Now()); v.State != "unavailable" {
		t.Fatalf("non-messages or local response updated the snapshot: %+v", v)
	}
}

func limitsHeader(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestAnthropicLimitsViewStates(t *testing.T) {
	captureLog(t)
	now := time.Now()
	with := limitsHeader("Anthropic-Ratelimit-Example-Status", "allowed")

	l := newAnthropicLimits("", 30*time.Minute)
	if v := l.view(now); v.State != "unavailable" || !v.ObservedAt.IsZero() || v.AgeSeconds != nil || v.MaxAgeSeconds != 1800 || v.Windows != nil || v.Raw != nil {
		t.Fatalf("nothing observed: %+v", v)
	}
	l.observe(http.Header{}, now.Add(-10*time.Minute))
	if v := l.view(now); v.State != "no_headers" || !v.ObservedAt.Equal(now.Add(-10*time.Minute)) || v.AgeSeconds == nil || *v.AgeSeconds != 600 || v.Windows != nil || v.Raw != nil {
		t.Fatalf("response without headers: %+v", v)
	}
	l.observe(with, now.Add(-30*time.Minute))
	if v := l.view(now); v.State != "fresh" || *v.AgeSeconds != 1800 || limitKeys(v) != "example" {
		t.Fatalf("headers exactly max age old are still current: %+v", v)
	}
	if v := l.view(now.Add(time.Second)); v.State != "no_headers" {
		t.Fatalf("stale headers must not be shown as current: %+v", v)
	}
	if v := l.view(now.Add(21 * time.Minute)); v.State != "unavailable" || !v.ObservedAt.Equal(now.Add(-10*time.Minute)) || *v.AgeSeconds != 31*60 || v.Windows != nil || v.Raw != nil {
		t.Fatalf("everything stale: %+v", v)
	}

	// A later answer without the headers (an error, say) does not hide a
	// current snapshot.
	l.observe(with, now.Add(-time.Minute))
	l.observe(http.Header{}, now)
	if v := l.view(now); v.State != "fresh" || !v.ObservedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("newer response without headers hid current snapshot: %+v", v)
	}
}

func TestAnthropicLimitsIgnoresOlderObservation(t *testing.T) {
	captureLog(t)
	now := time.Now()
	l := newAnthropicLimits("", anthropicLimitsMaxAge)
	l.observe(limitsHeader("Anthropic-Ratelimit-New", "1"), now)
	l.observe(limitsHeader("Anthropic-Ratelimit-Old", "1"), now.Add(-time.Second))
	if v := l.view(now); limitKeys(v) != "anthropic-ratelimit-new" || !v.ObservedAt.Equal(now) {
		t.Fatalf("older response replaced newer snapshot: %+v", v)
	}
	l2 := newAnthropicLimits("", anthropicLimitsMaxAge)
	l2.observe(http.Header{}, now)
	l2.observe(http.Header{}, now.Add(-time.Second))
	if v := l2.view(now); !v.ObservedAt.Equal(now) {
		t.Fatalf("older response moved time back: %+v", v)
	}
}

// A slow streaming response may reach the hook minutes after a newer one.
// Its timestamp must not make the newer observation look like an invalid
// future timestamp just because it was used as the clock for the comparison.
func TestAnthropicLimitsSlowOlderResponseDoesNotRegress(t *testing.T) {
	captureLog(t)
	now := time.Now()
	l := newAnthropicLimits("", anthropicLimitsMaxAge)
	l.observe(limitsHeader("Anthropic-Ratelimit-New", "1"), now)
	l.observe(limitsHeader("Anthropic-Ratelimit-Old", "1"), now.Add(-5*time.Minute))
	if v := l.view(now); !v.ObservedAt.Equal(now) || limitKeys(v) != "anthropic-ratelimit-new" {
		t.Fatalf("slow response replaced newer headers: %+v", v)
	}

	l2 := newAnthropicLimits("", anthropicLimitsMaxAge)
	l2.observe(http.Header{}, now)
	l2.observe(http.Header{}, now.Add(-5*time.Minute))
	if v := l2.view(now); !v.ObservedAt.Equal(now) {
		t.Fatalf("slow response moved no-headers time back: %+v", v)
	}
}

// A timestamp from the future (clock moved back, hand-edited file) must
// neither look current nor block real observations.
func TestAnthropicLimitsFutureTimestamp(t *testing.T) {
	captureLog(t)
	now := time.Now()
	l := newAnthropicLimits("", anthropicLimitsMaxAge)
	l.observe(limitsHeader("Anthropic-Ratelimit-Future", "1"), now.Add(2*time.Hour))
	if v := l.view(now); v.State != "unavailable" || !v.ObservedAt.IsZero() || v.AgeSeconds != nil {
		t.Fatalf("future snapshot shown: %+v", v)
	}
	l.observe(limitsHeader("Anthropic-Ratelimit-Now", "1"), now)
	if v := l.view(now); v.State != "fresh" || limitKeys(v) != "anthropic-ratelimit-now" {
		t.Fatalf("future snapshot blocked a real one: %+v", v)
	}
}

func TestAnthropicLimitsPersistRoundTrip(t *testing.T) {
	captureLog(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "limits.json")
	now := time.Now()
	l := newAnthropicLimits(path, anthropicLimitsMaxAge)
	t.Cleanup(l.close)
	l.observe(limitsHeader("Anthropic-Ratelimit-A", "value-a"), now.Add(-time.Minute))
	l.observe(http.Header{}, now.Add(-2*time.Minute))
	if err := l.save(); err != nil {
		t.Fatal(err)
	}
	requirePrivateFile(t, path)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "limits.json" && e.Name() != "limits.json.lock" {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
	var disk limitsState
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &disk); err != nil || disk.WithHeaders == nil || disk.WithHeaders.Headers["anthropic-ratelimit-a"] != "value-a" || !disk.WithoutAt.Equal(now.Add(-2*time.Minute)) {
		t.Fatalf("file content %s: %v", data, err)
	}

	again := newAnthropicLimits(path, anthropicLimitsMaxAge)
	a, _ := json.Marshal(l.view(now))
	b, _ := json.Marshal(again.view(now))
	if string(a) != string(b) {
		t.Fatalf("after restart %s, before %s", b, a)
	}
}

// Two router processes share limits.json during a deploy: a save never
// replaces a newer observation on disk with an older one.
func TestAnthropicLimitsSaveKeepsNewerDiskState(t *testing.T) {
	captureLog(t)
	path := filepath.Join(t.TempDir(), "limits.json")
	now := time.Now()
	older := newAnthropicLimits(path, anthropicLimitsMaxAge)
	t.Cleanup(older.close)
	newer := newAnthropicLimits(path, anthropicLimitsMaxAge)
	t.Cleanup(newer.close)
	older.observe(limitsHeader("Anthropic-Ratelimit-Old", "1"), now.Add(-5*time.Minute))
	newer.observe(limitsHeader("Anthropic-Ratelimit-New", "1"), now.Add(-time.Minute))
	newer.observe(http.Header{}, now)
	if err := newer.save(); err != nil {
		t.Fatal(err)
	}
	if err := older.save(); err != nil {
		t.Fatal(err)
	}
	reread := newAnthropicLimits(path, anthropicLimitsMaxAge)
	for name, l := range map[string]*anthropicLimits{"file": reread, "stale process": older} {
		if v := l.view(now); limitKeys(v) != "anthropic-ratelimit-new" {
			t.Fatalf("%s regressed to older snapshot: %+v", name, v)
		}
	}
	var disk limitsState
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &disk); err != nil || !disk.WithoutAt.Equal(now) {
		t.Fatalf("without_at regressed: %s %v", data, err)
	}
}

// holdLock holds the file lock at path, as another router process would.
// The returned func releases it; the end of the test releases it too.
func holdLock(t *testing.T, path string) (release func()) {
	t.Helper()
	held, stop, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- platform.WithLock(context.Background(), path, func() error {
			close(held)
			<-stop
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("hold lock: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			close(stop)
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(release)
	return release
}

// The read-merge-write of one process must not interleave with another's.
func TestAnthropicLimitsSaveWaitsForOtherWriter(t *testing.T) {
	captureLog(t)
	path := filepath.Join(t.TempDir(), "limits.json")
	release := holdLock(t, path+".lock")
	l := newAnthropicLimits(path, anthropicLimitsMaxAge)
	t.Cleanup(l.close)
	l.observe(limitsHeader("Anthropic-Ratelimit-A", "1"), time.Now())
	saved := make(chan error, 1)
	go func() { saved <- l.save() }()
	select {
	case err := <-saved:
		t.Fatalf("saved while another writer held the lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("written while another writer held the lock")
	}
	release()
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
}

// A writer that keeps the lock makes a save fail after limitsLockWait, with
// the lock file named, instead of blocking the saver for good.
func TestAnthropicLimitsSaveGivesUpOnHeldLock(t *testing.T) {
	captureLog(t)
	path := filepath.Join(t.TempDir(), "limits.json")
	holdLock(t, path+".lock")
	l := newAnthropicLimits(path, anthropicLimitsMaxAge)
	t.Cleanup(l.close)
	start := time.Now()
	err := l.save()
	if err == nil || err.Error() != path+".lock: held by another process" {
		t.Fatalf("save under a held lock: %v", err)
	}
	if waited := time.Since(start); waited < limitsLockWait || waited > limitsLockWait+time.Second {
		t.Fatalf("waited %v, want about %v", waited, limitsLockWait)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("written while another writer held the lock")
	}
}

func TestAnthropicLimitsBadFile(t *testing.T) {
	buf := captureLog(t)
	dir := t.TempDir()
	missing := newAnthropicLimits(filepath.Join(dir, "missing.json"), anthropicLimitsMaxAge)
	if v := missing.view(time.Now()); v.State != "unavailable" {
		t.Fatalf("missing file: %+v", v)
	}
	missing.close()
	if _, err := os.Stat(filepath.Join(dir, "missing.json")); !os.IsNotExist(err) {
		t.Fatal("file created without an observation")
	}
	if buf.Len() != 0 {
		t.Fatalf("missing file reported: %q", buf.String())
	}

	path := filepath.Join(dir, "limits.json")
	writeRaw(t, path, "{not json")
	l := newAnthropicLimits(path, anthropicLimitsMaxAge)
	t.Cleanup(l.close)
	if v := l.view(time.Now()); v.State != "unavailable" {
		t.Fatalf("corrupt file: %+v", v)
	}
	if !strings.Contains(buf.String(), "anthropic limits: "+path) {
		t.Fatalf("corrupt file not reported: %q", buf.String())
	}
	l.observe(limitsHeader("Anthropic-Ratelimit-A", "1"), time.Now())
	if err := l.save(); err != nil {
		t.Fatal(err)
	}
	if v := newAnthropicLimits(path, anthropicLimitsMaxAge).view(time.Now()); v.State != "fresh" {
		t.Fatalf("corrupt file not replaced: %+v", v)
	}

	// A hand-edited file cannot smuggle other headers or unbounded names in.
	edited := filepath.Join(dir, "edited.json")
	writeRaw(t, edited, `{"with_headers":{"at":"`+time.Now().Format(time.RFC3339Nano)+`","headers":{"authorization":"x","anthropic-ratelimit-ok":"1","anthropic-ratelimit-`+strings.Repeat("n", 500)+`":"1"}}}`)
	if v := newAnthropicLimits(edited, anthropicLimitsMaxAge).view(time.Now()); limitKeys(v) != "anthropic-ratelimit-ok" {
		t.Fatalf("file headers not filtered: %+v", v)
	}
}

func TestAnthropicLimitsSaveErrorKeepsMemory(t *testing.T) {
	buf := captureLog(t)
	notDir := filepath.Join(t.TempDir(), "file")
	writeRaw(t, notDir, "")
	l := newAnthropicLimits(filepath.Join(notDir, "limits.json"), anthropicLimitsMaxAge)
	t.Cleanup(l.close)
	l.observe(limitsHeader("Anthropic-Ratelimit-A", "secret-value"), time.Now())
	if err := l.save(); err == nil {
		t.Fatal("save into a file path succeeded")
	}
	if v := l.view(time.Now()); v.State != "fresh" {
		t.Fatalf("failed save lost the snapshot: %+v", v)
	}
	l.observe(limitsHeader("Anthropic-Ratelimit-A", "secret-value"), time.Now().Add(time.Second))
	l.close()
	out := buf.String()
	if strings.Count(out, "anthropic limits: save") != 1 || strings.Contains(out, "secret-value") {
		t.Fatalf("save failure must be logged once, without values: %q", out)
	}
}

// The proxy never waits for the disk: observe only schedules a save, and
// close flushes whatever is pending.
func TestAnthropicLimitsBackgroundSave(t *testing.T) {
	captureLog(t)
	path := filepath.Join(t.TempDir(), "limits.json")
	l := newAnthropicLimits(path, anthropicLimitsMaxAge)
	l.observe(limitsHeader("Anthropic-Ratelimit-A", "1"), time.Now())
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("observation never saved")
		}
		time.Sleep(5 * time.Millisecond)
	}
	l.observe(limitsHeader("Anthropic-Ratelimit-B", "1"), time.Now().Add(time.Second))
	l.close()
	l.close()
	if v := newAnthropicLimits(path, anthropicLimitsMaxAge).view(time.Now().Add(time.Second)); limitKeys(v) != "anthropic-ratelimit-b" {
		t.Fatalf("close did not flush the last observation: %+v", v)
	}
	l.observe(http.Header{}, time.Now().Add(2*time.Second)) // after close: memory only
	var nilStore *anthropicLimits
	nilStore.close()
	if err := nilStore.save(); err != nil {
		t.Fatal(err)
	}
}

func TestAnthropicLimitsConcurrentUse(t *testing.T) {
	captureLog(t)
	l := newAnthropicLimits(filepath.Join(t.TempDir(), "limits.json"), anthropicLimitsMaxAge)
	t.Cleanup(l.close)
	start := time.Now()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				at := start.Add(time.Duration(i*8+g) * time.Millisecond)
				if i%2 == 0 {
					l.observe(limitsHeader(fmt.Sprintf("Anthropic-Ratelimit-G%d", g), "1"), at)
				} else {
					l.observe(http.Header{}, at)
				}
				l.view(at)
				if i%50 == 0 {
					_ = l.save() // racing writers; the view below is what counts
				}
			}
		}(g)
	}
	wg.Wait()
	last := start.Add(time.Duration(199*8+7) * time.Millisecond)
	if v := l.view(last); v.State != "fresh" || !v.ObservedAt.Equal(start.Add(time.Duration(198*8+7)*time.Millisecond)) {
		t.Fatalf("after concurrent use: %+v", v)
	}
}

// Every header is anthropic-ratelimit-<window>-<field>, the field being
// utilization, remaining, limit, reset or status. One function reads them all
// without knowing window names; anything else stays raw under its full name.
func TestParseLimitWindows(t *testing.T) {
	windows, raw := parseLimitWindows(map[string]string{
		"anthropic-ratelimit-unified-5h-utilization":       "0.23",
		"anthropic-ratelimit-unified-5h-reset":             "1790348400",
		"anthropic-ratelimit-unified-7d-utilization":       "1.25",
		"anthropic-ratelimit-unified-7d-status":            "rejected",
		"anthropic-ratelimit-unified-7d_oi-utilization":    "0.1234",
		"anthropic-ratelimit-unified-status":               "allowed_warning",
		"anthropic-ratelimit-requests-limit":               "50",
		"anthropic-ratelimit-requests-remaining":           "41",
		"anthropic-ratelimit-requests-reset":               "2026-09-25T18:00:00+03:00",
		"anthropic-ratelimit-tokens-remaining":             "39999",
		"anthropic-ratelimit-both-utilization":             "0.5",
		"anthropic-ratelimit-both-remaining":               "9",
		"anthropic-ratelimit-both-limit":                   "10",
		"anthropic-ratelimit-over-remaining":               "15",
		"anthropic-ratelimit-over-limit":                   "10",
		"anthropic-ratelimit-zero-remaining":               "0",
		"anthropic-ratelimit-zero-limit":                   "0",
		"anthropic-ratelimit-unified-representative-claim": "five_hour",
		"anthropic-ratelimit-unified-fallback":             "available",
		"anthropic-ratelimit-reset":                        "1790348400",
	})
	got, _ := json.Marshal(windows)
	want := `[` +
		`{"name":"both","remaining_percent":50,"used_percent":50,"remaining":9,"limit":10},` +
		`{"name":"over","remaining_percent":100,"remaining":15,"limit":10},` +
		`{"name":"requests","remaining_percent":82,"remaining":41,"limit":50,"reset_at":"2026-09-25T15:00:00Z"},` +
		`{"name":"tokens","remaining":39999},` +
		`{"name":"unified","status":"allowed_warning"},` +
		`{"name":"unified-5h","remaining_percent":77,"used_percent":23,"reset_at":"2026-09-25T15:00:00Z"},` +
		`{"name":"unified-7d","remaining_percent":0,"used_percent":125,"status":"rejected"},` +
		`{"name":"unified-7d_oi","remaining_percent":87.7,"used_percent":12.3},` +
		`{"name":"zero","remaining":0,"limit":0}` +
		`]`
	if string(got) != want {
		t.Fatalf("windows\n got %s\nwant %s", got, want)
	}
	wantRaw := map[string]string{
		"anthropic-ratelimit-unified-representative-claim": "five_hour",
		"anthropic-ratelimit-unified-fallback":             "available",
		"anthropic-ratelimit-reset":                        "1790348400",
	}
	if !maps.Equal(raw, wantRaw) {
		t.Fatalf("raw %v, want %v", raw, wantRaw)
	}
}

// A value that does not parse stays raw under its full name; nothing is
// guessed from it.
func TestParseLimitWindowsMalformed(t *testing.T) {
	for _, c := range []struct{ name, value string }{
		{"anthropic-ratelimit-unified-5h-utilization", "abc"},
		{"anthropic-ratelimit-unified-5h-utilization", "-0.1"},
		{"anthropic-ratelimit-unified-5h-utilization", "NaN"},
		{"anthropic-ratelimit-unified-5h-utilization", "+Inf"},
		{"anthropic-ratelimit-unified-5h-utilization", "23%"},
		{"anthropic-ratelimit-requests-remaining", "-1"},
		{"anthropic-ratelimit-requests-remaining", "4.5"},
		{"anthropic-ratelimit-requests-limit", "many"},
		{"anthropic-ratelimit-unified-5h-reset", "tomorrow"},
		{"anthropic-ratelimit-unified-5h-reset", "0"},
		{"anthropic-ratelimit-unified-5h-reset", "1790348400000"}, // milliseconds are not guessed
		{"anthropic-ratelimit-unified-5h-status", ""},
		{"anthropic-ratelimit--utilization", "0.5"},
		{"anthropic-ratelimit-utilization", "0.5"},
	} {
		windows, raw := parseLimitWindows(map[string]string{c.name: c.value})
		if v, ok := raw[c.name]; len(windows) != 0 || len(raw) != 1 || !ok || v != c.value {
			t.Fatalf("%s=%q: windows %+v, raw %v", c.name, c.value, windows, raw)
		}
	}

	windows, raw := parseLimitWindows(map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "0.5",
		"anthropic-ratelimit-unified-5h-reset":       "soon",
	})
	if len(windows) != 1 || windows[0].Name != "unified-5h" || !windows[0].ResetAt.IsZero() || raw["anthropic-ratelimit-unified-5h-reset"] != "soon" {
		t.Fatalf("one bad field must not drop the window: %+v %v", windows, raw)
	}

	if windows, raw := parseLimitWindows(nil); len(windows) != 0 || len(raw) != 0 {
		t.Fatalf("no headers: %+v %v", windows, raw)
	}
}

// /api/limits and the settings page show this JSON; docs/features.md
// describes it, so a change here is a change there.
func TestAnthropicLimitsViewJSON(t *testing.T) {
	captureLog(t)
	now := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	at := now.Format(time.RFC3339)
	l := newAnthropicLimits("", anthropicLimitsMaxAge)
	l.observe(limitsHeader(
		"Anthropic-Ratelimit-Unified-5h-Utilization", "0.23",
		"Anthropic-Ratelimit-Unified-5h-Reset", "1790348400",
		"Anthropic-Ratelimit-Unified-Status", "allowed",
		"Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour",
	), now)
	report := func(l *anthropicLimits, at time.Time) string {
		got, _ := json.Marshal(limitsReportOf(l.view(at), nil, at))
		return string(got)
	}
	const noCodex = `{"source":"codex","state":"not_connected","max_age_seconds":1800}`
	for _, c := range []struct {
		after time.Duration
		want  string
	}{
		{90 * time.Second, `{"sources":[{"source":"anthropic","state":"fresh","observed_at":"` + at + `","age_seconds":90,"max_age_seconds":1800,` +
			`"raw":{"anthropic-ratelimit-unified-representative-claim":"five_hour"}},` + noCodex + `],` +
			`"windows":[{"source":"anthropic","name":"unified","status":"allowed","observed_at":"` + at + `"},` +
			`{"source":"anthropic","name":"unified-5h","remaining_percent":77,"used_percent":23,"reset_at":"2026-09-25T15:00:00Z","observed_at":"` + at + `"}]}`},
		{31 * time.Minute, `{"sources":[{"source":"anthropic","state":"unavailable","observed_at":"` + at + `","age_seconds":1860,"max_age_seconds":1800},` + noCodex + `],"windows":[]}`},
	} {
		if got := report(l, now.Add(c.after)); got != c.want {
			t.Fatalf("after %s\n got %s\nwant %s", c.after, got, c.want)
		}
	}

	// While fresh, raw is always there, even empty; windows always are.
	for _, c := range []struct {
		h    http.Header
		want string
	}{
		{limitsHeader("Anthropic-Ratelimit-Unified-5h-Utilization", "0.5"),
			`"raw":{}},` + noCodex + `],"windows":[{"source":"anthropic","name":"unified-5h","remaining_percent":50,"used_percent":50,"observed_at":"` + at + `"}]}`},
		{limitsHeader("Anthropic-Ratelimit-Unified-Fallback", "available"),
			`"raw":{"anthropic-ratelimit-unified-fallback":"available"}},` + noCodex + `],"windows":[]}`},
	} {
		l := newAnthropicLimits("", anthropicLimitsMaxAge)
		l.observe(c.h, now)
		if got := report(l, now); !strings.HasPrefix(got, `{"sources":[{"source":"anthropic","state":"fresh",`) || !strings.HasSuffix(got, c.want) {
			t.Fatalf("got %s, want suffix %s", got, c.want)
		}
	}
	none := newAnthropicLimits("", anthropicLimitsMaxAge)
	none.observe(http.Header{}, now)
	if got := report(none, now); got != `{"sources":[{"source":"anthropic","state":"no_headers","observed_at":"`+at+`","age_seconds":0,"max_age_seconds":1800},`+noCodex+`],"windows":[]}` {
		t.Fatalf("no headers: %s", got)
	}
}

// Codex windows come from the last successful usage request, in the same
// shape as Anthropic's, and only while that request is at most 30 minutes old.
func TestLimitsReportCodex(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var payload codexUsagePayload
	if err := json.Unmarshal([]byte(`{"plan_type":"pro",
		"rate_limit":{"allowed":true,"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_at":1790348400},
			"secondary_window":{"limit_window_seconds":604800}},
		"additional_rate_limits":[{"limit_name":"code_review","rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"limit_window_seconds":18000}}}]}`), &payload); err != nil {
		t.Fatal(err)
	}
	updated := now.Add(-2 * time.Minute)
	at := updated.Format(time.RFC3339)
	fresh := decodeCodexUsage(payload, codexAccount{ID: "acct"}, updated)
	codex := func(v codexUsageView, at time.Time) string {
		r := limitsReportOf(anthropicLimitsView{State: "unavailable", MaxAgeSeconds: 1800}, []codexUsageView{v}, at)
		sources, _ := json.Marshal(r.Sources[1])
		windows, _ := json.Marshal(r.Windows)
		return string(sources) + " " + string(windows)
	}

	want := `{"source":"codex","state":"fresh","observed_at":"` + at + `","age_seconds":120,"max_age_seconds":1800} ` +
		`[{"source":"codex","name":"primary","remaining_percent":80,"used_percent":20,"reset_at":"2026-09-25T15:00:00Z","window_seconds":18000,"observed_at":"` + at + `"},` +
		`{"source":"codex","name":"secondary","window_seconds":604800,"observed_at":"` + at + `"},` +
		`{"source":"codex","name":"code_review-primary","remaining_percent":0,"used_percent":100,"status":"rejected","window_seconds":18000,"observed_at":"` + at + `"}]`
	if got := codex(fresh, now); got != want {
		t.Fatalf("fresh\n got %s\nwant %s", got, want)
	}

	// A failed refresh keeps the last good numbers while they are fresh and
	// says why the newer ones are missing.
	failed := fresh
	failed.Error = "сервис лимитов Codex вернул HTTP 503"
	want = `{"source":"codex","state":"fresh","observed_at":"` + at + `","age_seconds":120,"max_age_seconds":1800,"error":"сервис лимитов Codex вернул HTTP 503"} [`
	if got := codex(failed, now); !strings.HasPrefix(got, want) {
		t.Fatalf("failed refresh\n got %s\nwant prefix %s", got, want)
	}

	for _, c := range []struct {
		name string
		v    codexUsageView
		at   time.Time
		want string
	}{
		{"stale", failed, updated.Add(31 * time.Minute),
			`{"source":"codex","state":"unavailable","observed_at":"` + at + `","age_seconds":1860,"max_age_seconds":1800,"error":"сервис лимитов Codex вернул HTTP 503"} []`},
		{"never fetched", codexUsageView{Connected: true, Error: "нет ответа"}, now,
			`{"source":"codex","state":"unavailable","max_age_seconds":1800,"error":"нет ответа"} []`},
		{"not connected", codexUsageView{}, now,
			`{"source":"codex","state":"not_connected","max_age_seconds":1800} []`},
	} {
		if got := codex(c.v, c.at); got != c.want {
			t.Fatalf("%s\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// Limit values live in limits.json and the view only: neither the request
// history nor the log gets them.
func TestAnthropicLimitsValuesStayOutOfHistoryAndLog(t *testing.T) {
	buf := captureLog(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.4242")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "1790348417")
		w.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", "claim-value-7931")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	histPath := filepath.Join(t.TempDir(), "history.jsonl")
	u, handler := limitsRouterStore(t, upstream.URL, "", history.New(10, histPath))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if v := u.limits.view(time.Now()); v.State != "fresh" || limitKeys(v) != "anthropic-ratelimit-unified-representative-claim,unified-5h" {
		t.Fatalf("snapshot %+v", v)
	}
	data, err := os.ReadFile(histPath)
	if err != nil || len(data) == 0 {
		t.Fatalf("history not written: %v", err)
	}
	// Start and End carry fractional seconds: at hh:mm:57.6… they would
	// contain "57.6" without any limit value in the record.
	data = regexp.MustCompile(`"(Start|End)":"[^"]*"`).ReplaceAll(data, nil)
	for _, value := range []string{"0.4242", "1790348417", "claim-value-7931", "42.4", "57.6"} {
		if bytes.Contains(data, []byte(value)) {
			t.Fatalf("limit value %s in history: %s", value, data)
		}
		if strings.Contains(buf.String(), value) {
			t.Fatalf("limit value %s in log: %s", value, buf.String())
		}
	}
}

func TestLimitsPath(t *testing.T) {
	t.Setenv("ROUTER_ANTHROPIC_LIMITS_FILE", "/x/limits.json")
	if p := limitsPath(); p != "/x/limits.json" {
		t.Fatal(p)
	}
	t.Setenv("ROUTER_ANTHROPIC_LIMITS_FILE", "")
	if p := limitsPath(); p != "" {
		t.Fatalf("empty override must disable persistence: %q", p)
	}
	os.Unsetenv("ROUTER_ANTHROPIC_LIMITS_FILE")
	exe, _ := os.Executable()
	if p := limitsPath(); p != filepath.Join(filepath.Dir(exe), "limits.json") {
		t.Fatal(p)
	}
}

// limitsSection is the Anthropic limits block of the settings page.
func limitsSection(t *testing.T, h http.Handler) string {
	t.Helper()
	body := get(t, h, "GET", "/settings", nil).Body.String()
	start := strings.Index(body, `<section class="settings-section" id="anthropic-limits">`)
	if start < 0 {
		t.Fatal("no Anthropic limits section on the settings page")
	}
	end := strings.Index(body[start:], "</section>")
	return body[start : start+end]
}

func TestAnthropicLimitsSettingsBlock(t *testing.T) {
	captureLog(t)
	u, h := testUI(t)
	page := get(t, h, "GET", "/settings", nil).Body.String()
	codex, limits, conns := strings.Index(page, `id="codex-subscription"`), strings.Index(page, `id="anthropic-limits"`), strings.Index(page, `id="connections"`)
	if codex < 0 || codex > limits || limits > conns {
		t.Fatalf("limits block is not between Codex subscription and connections: %d %d %d", codex, limits, conns)
	}
	if !strings.Contains(page, `<a href="#anthropic-limits">`) {
		t.Fatal("no navigation link to the limits block")
	}
	full := httptest.NewRecorder()
	h.ServeHTTP(full, httptest.NewRequest("GET", "/settings", nil))
	if full.Code != http.StatusOK || !strings.Contains(full.Body.String(), `id="anthropic-limits"`) || strings.Contains(full.Body.String(), "<no value>") {
		t.Fatalf("full settings page: %d", full.Code)
	}

	now := time.Now()
	reset := now.Add(90*time.Minute + 30*time.Second).Truncate(time.Second)
	withValues := limitsHeader(
		"Anthropic-Ratelimit-Unified-5h-Utilization", "0.23",
		"Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(reset.Unix(), 10),
		"Anthropic-Ratelimit-Unified-7d-Utilization", "0.955",
		"Anthropic-Ratelimit-Unified-Status", "allowed",
		"Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour",
	)
	for _, c := range []struct {
		name       string
		setup      func(*anthropicLimits)
		want, deny []string
	}{
		{"nothing observed", func(*anthropicLimits) {},
			[]string{"Недоступно", "ещё не видел ответов Anthropic"}, []string{"осталось"}},
		{"no headers", func(l *anthropicLimits) { l.observe(http.Header{}, now) },
			[]string{"Anthropic не присылает лимиты в ответах"}, []string{"Недоступно", "осталось"}},
		{"fresh", func(l *anthropicLimits) { l.observe(withValues, now) },
			[]string{"<h4>unified-5h</h4>", "77% <span>осталось</span>", "Использовано 23%", `<progress max="100" value="77"`,
				`Сброс: <time datetime="` + reset.UTC().Format(time.RFC3339) + `"`, "через 1 ч. 30 мин.",
				"4.5% <span>осталось</span>", "quota-low", "Статус: <code>allowed</code>", "Данные получены",
				`<details class="settings-extra">`, "<code>anthropic-ratelimit-unified-representative-claim</code>", "five_hour"},
			[]string{"Недоступно", "<no value>", "%!"}},
		{"unknown headers only", func(l *anthropicLimits) {
			l.observe(limitsHeader("Anthropic-Ratelimit-Unified-Fallback", "available"), now)
		},
			[]string{"Окон лимитов в заголовках нет", "<code>anthropic-ratelimit-unified-fallback</code>", "available"},
			[]string{"Недоступно", "<progress", "<no value>"}},
		{"stale", func(l *anthropicLimits) { l.observe(withValues, now.Add(-31*time.Minute)) },
			[]string{"Недоступно", "старше 30 мин"}, []string{"осталось", "unified-5h", "five_hour", "<progress"}},
	} {
		u.limits = newAnthropicLimits("", anthropicLimitsMaxAge)
		c.setup(u.limits)
		s := limitsSection(t, h)
		for _, w := range c.want {
			if !strings.Contains(s, w) {
				t.Fatalf("%s: %q missing in\n%s", c.name, w, s)
			}
		}
		for _, d := range c.deny {
			if strings.Contains(s, d) {
				t.Fatalf("%s: %q must not appear in\n%s", c.name, d, s)
			}
		}
		if !strings.Contains(s, "Сам роутер к Anthropic не обращается") || strings.Contains(s, "<button") || strings.Contains(s, "hx-") {
			t.Fatalf("%s: the block must be passive and say so:\n%s", c.name, s)
		}
	}
}

func TestAnthropicLimitsAPI(t *testing.T) {
	captureLog(t)
	u, h := testUI(t)
	out := countOutbound(t)
	now := time.Now()
	u.limits.observe(limitsHeader(
		"Anthropic-Ratelimit-Unified-5h-Utilization", "0.23",
		"Anthropic-Ratelimit-Unified-5h-Reset", "1790348400",
		"Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour",
	), now)

	// Codex is connected and its last refresh failed: its source says so and
	// the Anthropic part is unaffected.
	codex := provider{Name: "codex", Type: "codex", BaseURL: codexBaseURL}
	u.cs.c.local.Providers = append(u.cs.c.local.Providers, codex)
	oldAuth := codexAuth
	codexAuth = &codexAuthStore{loaded: true, credential: usageCredential("acct")}
	t.Cleanup(func() { codexAuth = oldAuth })
	var codexCalls atomic.Int64
	u.codexUsage.client = &http.Client{Transport: usageTransport(func(*http.Request) (*http.Response, error) {
		codexCalls.Add(1)
		return usageResponse(503, "private-body"), nil
	})}
	u.usageCache(codex).get(t.Context(), codexAuth, true)

	w := get(t, h, "GET", "/api/limits", nil)
	if n := out.n.Load(); n != 0 || codexCalls.Load() != 1 {
		t.Fatalf("GET /api/limits made %d outbound requests, %d to Codex", n, codexCalls.Load()-1)
	}
	if ct, cc := w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"); ct != "application/json" || cc != "no-store" {
		t.Fatalf("Content-Type %q, Cache-Control %q", ct, cc)
	}
	var got limitsReport
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := limitsReportOf(u.limits.view(now), nil, now)
	gotWindows, _ := json.Marshal(got.Windows)
	wantWindows, _ := json.Marshal(want.Windows)
	if len(got.Sources) != 2 || got.Sources[0].Source != "anthropic" || got.Sources[0].State != "fresh" || !got.Sources[0].ObservedAt.Equal(want.Sources[0].ObservedAt) ||
		got.Sources[0].AgeSeconds == nil || got.Sources[0].MaxAgeSeconds != 1800 || !maps.Equal(got.Sources[0].Raw, want.Sources[0].Raw) ||
		string(gotWindows) != string(wantWindows) || len(got.Windows) != 1 {
		t.Fatalf("JSON %s, store %+v", w.Body.String(), want)
	}
	if c := got.Sources[1]; c.Source != "codex" || c.Provider != "codex" || c.State != "unavailable" || c.Error == "" || strings.Contains(w.Body.String(), "private-body") {
		t.Fatalf("codex source: %s", w.Body.String())
	}
	if s := limitsSection(t, h); !strings.Contains(s, "77% <span>осталось</span>") || !strings.Contains(s, "five_hour") {
		t.Fatalf("page disagrees with JSON:\n%s", s)
	}

	for _, method := range []string{"POST", "PUT", "DELETE"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/api/limits", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /api/limits: %d", method, rec.Code)
		}
	}

	u.limits = newAnthropicLimits("", anthropicLimitsMaxAge)
	codexAuth = &codexAuthStore{path: filepath.Join(t.TempDir(), "missing.json")}
	if body := get(t, h, "GET", "/api/limits", nil).Body.String(); !strings.HasPrefix(body,
		`{"sources":[{"source":"anthropic","state":"unavailable","max_age_seconds":1800},{"source":"codex","provider":"codex","state":"not_connected","max_age_seconds":1800}],"windows":[]}`) {
		t.Fatalf("signed out: %s", body)
	}
	// With no Codex connection at all the report still has one codex source.
	u.cs.c.local.Providers = u.cs.c.local.Providers[:len(u.cs.c.local.Providers)-1]
	if body := get(t, h, "GET", "/api/limits", nil).Body.String(); !strings.HasPrefix(body,
		`{"sources":[{"source":"anthropic","state":"unavailable","max_age_seconds":1800},{"source":"codex","state":"not_connected","max_age_seconds":1800}],"windows":[]}`) {
		t.Fatalf("nothing observed: %s", body)
	}
}

// Every Codex connection is its own codex source in /api/limits, named by
// provider and listed in providers order; its windows carry the same name.
func TestCodexLimitsPerConnection(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	seedConnection(t, provider{Name: "codex", Type: "codex"}, "acct-a")
	seedConnection(t, provider{Name: "work", Type: "codex", AuthID: testAuthB}, "acct-b")
	providers := []provider{{Name: "codex", Type: "codex", BaseURL: codexBaseURL}, {Name: "work", Type: "codex", BaseURL: codexBaseURL, AuthID: testAuthB}}
	u := newUIServer(history.New(10, ""), newConfigStore(config{local: localSetup{Providers: providers}}, ""), newHealth(""))
	u.limits = newAnthropicLimits("", anthropicLimitsMaxAge)
	used := map[string]string{"acct-a": "20", "acct-b": "70"}
	u.codexUsage.client = &http.Client{Transport: usageTransport(func(r *http.Request) (*http.Response, error) {
		return usageResponse(200, `{"rate_limit":{"primary_window":{"used_percent":`+used[r.Header.Get("ChatGPT-Account-Id")]+`}}}`), nil
	})}
	for _, p := range providers {
		store, err := codexStoreFor(p)
		if err != nil {
			t.Fatal(err)
		}
		u.usageCache(p).get(t.Context(), store, true)
	}

	body := get(t, u.handler(), "GET", "/api/limits", nil).Body.Bytes()
	var got limitsReport
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	var sources, windows []string
	for _, s := range got.Sources {
		sources = append(sources, s.Source+"/"+s.Provider+"/"+s.State)
	}
	for _, w := range got.Windows {
		percent := "-"
		if w.UsedPercent != nil {
			percent = fmt.Sprint(*w.UsedPercent)
		}
		windows = append(windows, w.Source+"/"+w.Provider+"/"+w.Name+"/"+percent)
	}
	if strings.Join(sources, ",") != "anthropic//unavailable,codex/codex/fresh,codex/work/fresh" ||
		strings.Join(windows, ",") != "codex/codex/primary/20,codex/work/primary/70" {
		t.Fatalf("sources %v, windows %v\n%s", sources, windows, body)
	}
}
