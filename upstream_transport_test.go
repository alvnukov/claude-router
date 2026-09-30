package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// followDefault sends through whatever http.DefaultTransport is at the
// moment, so tests that fake the network there keep faking it.
type followDefault struct{}

func (followDefault) RoundTrip(r *http.Request) (*http.Response, error) {
	return http.DefaultTransport.RoundTrip(r)
}

// shippedUpstreamHTTP is upstreamHTTP as the router starts with it.
var shippedUpstreamHTTP http.RoundTripper

func TestMain(m *testing.M) {
	shippedUpstreamHTTP, upstreamHTTP = upstreamHTTP, followDefault{}
	os.Exit(m.Run())
}

func useUpstreamHTTP(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	prev := upstreamHTTP
	t.Cleanup(func() { upstreamHTTP = prev })
	upstreamHTTP = rt
}

// recordStreamMarks returns, in order, whether each request through
// upstreamHTTP went as streamed.
func recordStreamMarks(t *testing.T) func() []bool {
	t.Helper()
	var mu sync.Mutex
	var marks []bool
	next := upstreamHTTP
	useUpstreamHTTP(t, usageTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		marks = append(marks, streamed(r.Context()))
		mu.Unlock()
		return next.RoundTrip(r)
	}))
	return func() []bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(marks)
	}
}

// The router ships with both transports: a streamed request waits 30s for
// headers, any other as long as it takes; the rest is the same.
func TestUpstreamTransportSettings(t *testing.T) {
	shipped, ok := shippedUpstreamHTTP.(streamOrWhole)
	if !ok {
		t.Fatalf("upstreamHTTP is %T, want byStream(upstreamTransport())", shippedUpstreamHTTP)
	}
	for _, leg := range []struct {
		name    string
		rt      http.RoundTripper
		headers time.Duration
	}{{"streamed", shipped.stream, 30 * time.Second}, {"whole", shipped.whole, 0}} {
		tr, ok := leg.rt.(*http.Transport)
		if !ok {
			t.Fatalf("%s: %T, want *http.Transport", leg.name, leg.rt)
		}
		if tr.Proxy == nil || tr.DialContext == nil || !tr.ForceAttemptHTTP2 {
			t.Fatalf("%s: proxy, dialer or HTTP/2 missing: %+v", leg.name, tr)
		}
		if tr.TLSHandshakeTimeout != 10*time.Second || tr.ResponseHeaderTimeout != leg.headers || tr.IdleConnTimeout != 90*time.Second {
			t.Fatalf("%s: timeouts: tls %s headers %s idle %s", leg.name, tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout, tr.IdleConnTimeout)
		}
		if tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout != 30*time.Second || tr.HTTP2.PingTimeout != 15*time.Second {
			t.Fatalf("%s: HTTP/2 pings: %+v", leg.name, tr.HTTP2)
		}
	}
}

// What Anthropic receives through the proxy is byte for byte what it got
// over the default transport, streamed or not.
func TestUpstreamTransportKeepsProxiedRequests(t *testing.T) {
	captureLog(t)
	dumps := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dump, err := httputil.DumpRequest(r, true)
		if err != nil {
			t.Error(err)
		}
		dumps <- strings.ReplaceAll(strings.ReplaceAll(string(dump), "\r\n", "\n"), r.Host, "UPSTREAM")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	send := func(rt http.RoundTripper, body string) string {
		t.Helper()
		out := &countingTransport{next: rt}
		useUpstreamHTTP(t, out)
		_, handler := limitsRouter(t, upstream.URL, "")
		r := httptest.NewRequest("POST", "/v1/messages?beta=true", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer client-token")
		r.Header.Set("Anthropic-Version", "2023-06-01")
		r.Header.Set("Anthropic-Beta", "interleaved-thinking-2025-05-14")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("User-Agent", "claude-cli/2.1 (external, cli)")
		r.Header.Set("X-Stainless-Retry-Count", "0")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK || out.n.Load() != 1 {
			t.Fatalf("status %d, %d requests through upstreamHTTP", w.Code, out.n.Load())
		}
		return <-dumps
	}
	for _, stream := range []bool{true, false} {
		body := fmt.Sprintf(`{"model":"claude-sonnet-5","stream":%t,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, stream)
		before := send(http.DefaultTransport, body)
		after := send(byStream(upstreamTransport()), body)

		want := fmt.Sprintf(`POST /v1/messages?beta=true HTTP/1.1
Host: UPSTREAM
Accept-Encoding: gzip
Anthropic-Beta: interleaved-thinking-2025-05-14
Anthropic-Version: 2023-06-01
Authorization: Bearer client-token
Content-Length: %d
Content-Type: application/json
User-Agent: claude-cli/2.1 (external, cli)
X-Forwarded-For: 192.0.2.1
X-Stainless-Retry-Count: 0

`, len(body)) + body
		if before != want {
			t.Fatalf("stream %t, default transport, upstream got:\n%s\nwant:\n%s", stream, before, want)
		}
		if after != before {
			t.Fatalf("stream %t, upstream transport changed the request:\n%s\nwas:\n%s", stream, after, before)
		}
	}
}

func TestCodexUsesUpstreamTransport(t *testing.T) {
	// A local OpenAI model may think for minutes before its headers, so under
	// privacy it keeps http.DefaultTransport and no 30s header bound.
	t.Run("local OpenAI under privacy stays off it", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}))
		defer up.Close()
		var through atomic.Int32
		useUpstreamHTTP(t, usageTransport(func(r *http.Request) (*http.Response, error) {
			through.Add(1)
			return followDefault{}.RoundTrip(r)
		}))
		h, _, _ := trafficFixture(t, up, true)
		w := trafficCall(h, "/v1/messages", `{"model":"test","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusOK || through.Load() != 0 {
			t.Fatalf("status %d, %d requests through upstreamHTTP:\n%s", w.Code, through.Load(), w.Body.String())
		}
	})

	seedTwoConnections(t)
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "chatgpt.com" {
			t.Errorf("Codex request went around upstreamHTTP")
		}
		return usageResponse(400, `{"error":"invalid_grant"}`), nil
	})
	var hits atomic.Int32
	useUpstreamHTTP(t, usageTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "chatgpt.com" {
			return usageResponse(400, `{"error":"invalid_grant"}`), nil
		}
		hits.Add(1)
		return usageResponse(200, codexStreamOK), nil
	}))

	w, _ := runLocalRequest(t, context.Background(), twoCodexPool(), newHealth(""), "s1", true)
	if w.Code != http.StatusOK || hits.Load() != 1 {
		t.Fatalf("status %d, %d Codex requests through upstreamHTTP:\n%s", w.Code, hits.Load(), w.Body.String())
	}
}
