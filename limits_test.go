package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
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
