package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"strings"
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

func TestMain(m *testing.M) {
	upstreamHTTP = followDefault{}
	os.Exit(m.Run())
}

func useUpstreamHTTP(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	prev := upstreamHTTP
	t.Cleanup(func() { upstreamHTTP = prev })
	upstreamHTTP = rt
}

func TestUpstreamTransportSettings(t *testing.T) {
	tr := upstreamTransport()
	if tr.Proxy == nil || tr.DialContext == nil || !tr.ForceAttemptHTTP2 {
		t.Fatalf("proxy, dialer or HTTP/2 missing: %+v", tr)
	}
	if tr.TLSHandshakeTimeout != 10*time.Second || tr.ResponseHeaderTimeout != 30*time.Second || tr.IdleConnTimeout != 90*time.Second {
		t.Fatalf("timeouts: tls %s headers %s idle %s", tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout, tr.IdleConnTimeout)
	}
	if tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout != 30*time.Second || tr.HTTP2.PingTimeout != 15*time.Second {
		t.Fatalf("HTTP/2 pings: %+v", tr.HTTP2)
	}
}

// What Anthropic receives through the proxy is byte for byte what it got
// over the default transport.
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

	const body = `{"model":"claude-sonnet-5","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	send := func(rt http.RoundTripper) string {
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
	before := send(http.DefaultTransport)
	after := send(upstreamTransport())

	want := `POST /v1/messages?beta=true HTTP/1.1
Host: UPSTREAM
Accept-Encoding: gzip
Anthropic-Beta: interleaved-thinking-2025-05-14
Anthropic-Version: 2023-06-01
Authorization: Bearer client-token
Content-Length: 101
Content-Type: application/json
User-Agent: claude-cli/2.1 (external, cli)
X-Forwarded-For: 192.0.2.1
X-Stainless-Retry-Count: 0

` + body
	if before != want {
		t.Fatalf("default transport, upstream got:\n%s\nwant:\n%s", before, want)
	}
	if after != before {
		t.Fatalf("upstream transport changed the request:\n%s\nwas:\n%s", after, before)
	}
}

func TestCodexUsesUpstreamTransport(t *testing.T) {
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
