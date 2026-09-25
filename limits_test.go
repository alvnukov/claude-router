package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The test names here are synthetic: the real subscription header names are
// not confirmed, so nothing below claims what Anthropic sends.

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
	st := newStore(10, "")
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
		io.WriteString(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		io.WriteString(w, "event: message_stop\ndata: {}\n\n")
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
	if v.State != "unverified" || strings.Join(v.Headers, ",") != "anthropic-ratelimit-example-status,anthropic-ratelimit-requests-remaining" {
		t.Fatalf("snapshot %+v", v)
	}
}

// Only Anthropic's answers to /v1/messages count: a local route never reaches
// the proxy, and other proxied paths say nothing about the subscription.
func TestAnthropicLimitsIgnoresLocalAndOtherPaths(t *testing.T) {
	captureLog(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Example-Status", "allowed")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"input_tokens":1}`)
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
	if v := l.view(now); v.State != "unavailable" || !v.ObservedAt.IsZero() || v.AgeSeconds != nil || v.MaxAgeSeconds != 1800 || v.Headers != nil {
		t.Fatalf("nothing observed: %+v", v)
	}
	l.observe(http.Header{}, now.Add(-10*time.Minute))
	if v := l.view(now); v.State != "no_headers" || !v.ObservedAt.Equal(now.Add(-10*time.Minute)) || v.AgeSeconds == nil || *v.AgeSeconds != 600 || v.Headers != nil {
		t.Fatalf("response without headers: %+v", v)
	}
	l.observe(with, now.Add(-30*time.Minute))
	if v := l.view(now); v.State != "unverified" || *v.AgeSeconds != 1800 || strings.Join(v.Headers, ",") != "anthropic-ratelimit-example-status" {
		t.Fatalf("headers exactly max age old are still current: %+v", v)
	}
	if v := l.view(now.Add(time.Second)); v.State != "no_headers" {
		t.Fatalf("stale headers must not be shown as current: %+v", v)
	}
	if v := l.view(now.Add(21 * time.Minute)); v.State != "unavailable" || !v.ObservedAt.Equal(now.Add(-10*time.Minute)) || *v.AgeSeconds != 31*60 || v.Headers != nil {
		t.Fatalf("everything stale: %+v", v)
	}

	// A later answer without the headers (an error, say) does not hide a
	// current snapshot.
	l.observe(with, now.Add(-time.Minute))
	l.observe(http.Header{}, now)
	if v := l.view(now); v.State != "unverified" || !v.ObservedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("newer response without headers hid current snapshot: %+v", v)
	}
}

func TestAnthropicLimitsIgnoresOlderObservation(t *testing.T) {
	captureLog(t)
	now := time.Now()
	l := newAnthropicLimits("", anthropicLimitsMaxAge)
	l.observe(limitsHeader("Anthropic-Ratelimit-New", "1"), now)
	l.observe(limitsHeader("Anthropic-Ratelimit-Old", "1"), now.Add(-time.Second))
	if v := l.view(now); strings.Join(v.Headers, ",") != "anthropic-ratelimit-new" || !v.ObservedAt.Equal(now) {
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
	if v := l.view(now); !v.ObservedAt.Equal(now) || strings.Join(v.Headers, ",") != "anthropic-ratelimit-new" {
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
	if v := l.view(now); v.State != "unverified" || strings.Join(v.Headers, ",") != "anthropic-ratelimit-now" {
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
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("limits.json: %v %v", fi, err)
	}
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
		if v := l.view(now); strings.Join(v.Headers, ",") != "anthropic-ratelimit-new" {
			t.Fatalf("%s regressed to older snapshot: %+v", name, v)
		}
	}
	var disk limitsState
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &disk); err != nil || !disk.WithoutAt.Equal(now) {
		t.Fatalf("without_at regressed: %s %v", data, err)
	}
}

// The read-merge-write of one process must not interleave with another's.
func TestAnthropicLimitsSaveWaitsForOtherWriter(t *testing.T) {
	captureLog(t)
	path := filepath.Join(t.TempDir(), "limits.json")
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
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
	syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := <-saved; err != nil {
		t.Fatal(err)
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
	os.WriteFile(path, []byte("{not json"), 0o600)
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
	if v := newAnthropicLimits(path, anthropicLimitsMaxAge).view(time.Now()); v.State != "unverified" {
		t.Fatalf("corrupt file not replaced: %+v", v)
	}

	// A hand-edited file cannot smuggle other headers or unbounded names in.
	edited := filepath.Join(dir, "edited.json")
	os.WriteFile(edited, []byte(`{"with_headers":{"at":"`+time.Now().Format(time.RFC3339Nano)+`","headers":{"authorization":"x","anthropic-ratelimit-ok":"1","anthropic-ratelimit-`+strings.Repeat("n", 500)+`":"1"}}}`), 0o600)
	if v := newAnthropicLimits(edited, anthropicLimitsMaxAge).view(time.Now()); strings.Join(v.Headers, ",") != "anthropic-ratelimit-ok" {
		t.Fatalf("file headers not filtered: %+v", v)
	}
}

func TestAnthropicLimitsSaveErrorKeepsMemory(t *testing.T) {
	buf := captureLog(t)
	notDir := filepath.Join(t.TempDir(), "file")
	os.WriteFile(notDir, nil, 0o600)
	l := newAnthropicLimits(filepath.Join(notDir, "limits.json"), anthropicLimitsMaxAge)
	t.Cleanup(l.close)
	l.observe(limitsHeader("Anthropic-Ratelimit-A", "secret-value"), time.Now())
	if err := l.save(); err == nil {
		t.Fatal("save into a file path succeeded")
	}
	if v := l.view(time.Now()); v.State != "unverified" {
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
	if v := newAnthropicLimits(path, anthropicLimitsMaxAge).view(time.Now().Add(time.Second)); strings.Join(v.Headers, ",") != "anthropic-ratelimit-b" {
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
					l.save()
				}
			}
		}(g)
	}
	wg.Wait()
	last := start.Add(time.Duration(199*8+7) * time.Millisecond)
	if v := l.view(last); v.State != "unverified" || !v.ObservedAt.Equal(start.Add(time.Duration(198*8+7)*time.Millisecond)) {
		t.Fatalf("after concurrent use: %+v", v)
	}
}

// The documented API rate-limit headers describe organisation limits, not the
// subscription: they are named, never turned into numbers.
func TestAnthropicLimitsGenericHeadersStayUnverified(t *testing.T) {
	captureLog(t)
	l := newAnthropicLimits("", anthropicLimitsMaxAge)
	now := time.Now()
	l.observe(limitsHeader(
		"Anthropic-Ratelimit-Requests-Limit", "50",
		"Anthropic-Ratelimit-Requests-Remaining", "41",
		"Anthropic-Ratelimit-Requests-Reset", "2026-09-25T10:00:00Z",
		"Anthropic-Ratelimit-Tokens-Remaining", "39999",
	), now)
	v := l.view(now)
	if v.State != "unverified" {
		t.Fatalf("generic headers: %+v", v)
	}
	data, _ := json.Marshal(v)
	var keys map[string]json.RawMessage
	json.Unmarshal(data, &keys)
	for k := range keys {
		switch k {
		case "state", "observed_at", "age_seconds", "max_age_seconds", "headers":
		default:
			t.Fatalf("unexpected field %q in %s", k, data)
		}
	}
	for _, value := range []string{"50", "41", "39999", "2026-09-25T10:00:00Z"} {
		if strings.Contains(string(keys["headers"]), value) || strings.Contains(string(data), `"`+value+`"`) {
			t.Fatalf("header value %s leaked: %s", value, data)
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
	withValues := limitsHeader(
		"Anthropic-Ratelimit-Requests-Remaining", "value-41-secret",
		"Anthropic-Ratelimit-Example-Status", "value-allowed-secret",
	)
	for _, c := range []struct {
		name       string
		setup      func(*anthropicLimits)
		want, deny []string
	}{
		{"nothing observed", func(*anthropicLimits) {},
			[]string{"Недоступно", "ещё не видел ответов Anthropic"}, []string{"Не подтверждено"}},
		{"no headers", func(l *anthropicLimits) { l.observe(http.Header{}, now) },
			[]string{"Anthropic не присылает лимиты в ответах"}, []string{"Недоступно", "Не подтверждено"}},
		{"unverified", func(l *anthropicLimits) { l.observe(withValues, now) },
			[]string{"Не подтверждено", "<code>anthropic-ratelimit-example-status</code>", "<code>anthropic-ratelimit-requests-remaining</code>"},
			[]string{"value-", "<progress", "%", "Недоступно"}},
		{"stale", func(l *anthropicLimits) { l.observe(withValues, now.Add(-31*time.Minute)) },
			[]string{"Недоступно", "старше 30 мин"}, []string{"anthropic-ratelimit-example-status", "value-", "Не подтверждено"}},
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
	u.limits.observe(limitsHeader("Anthropic-Ratelimit-Example-Status", "value-secret"), now)

	w := get(t, h, "GET", "/api/limits", nil)
	if n := out.n.Load(); n != 0 {
		t.Fatalf("GET /api/limits made %d outbound requests", n)
	}
	if ct, cc := w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"); ct != "application/json" || cc != "no-store" {
		t.Fatalf("Content-Type %q, Cache-Control %q", ct, cc)
	}
	if strings.Contains(w.Body.String(), "value-secret") {
		t.Fatalf("header value in JSON: %s", w.Body.String())
	}
	var got anthropicLimitsView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := u.limits.view(now)
	if got.State != want.State || !got.ObservedAt.Equal(want.ObservedAt) || got.AgeSeconds == nil || got.MaxAgeSeconds != want.MaxAgeSeconds || strings.Join(got.Headers, ",") != strings.Join(want.Headers, ",") {
		t.Fatalf("JSON %+v, store %+v", got, want)
	}
	if s := limitsSection(t, h); !strings.Contains(s, "Не подтверждено") || !strings.Contains(s, "anthropic-ratelimit-example-status") {
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
	if body := get(t, h, "GET", "/api/limits", nil).Body.String(); !strings.HasPrefix(body, `{"state":"unavailable","max_age_seconds":1800}`) {
		t.Fatalf("nothing observed: %s", body)
	}
}
