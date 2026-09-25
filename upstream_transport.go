package main

import (
	"context"
	"net"
	"net/http"
	"time"
)

// upstreamHTTP carries every request to Anthropic and Codex; the tests point
// it back at http.DefaultTransport, where they fake the network.
var upstreamHTTP = byStream(upstreamTransport())

// upstreamTransport bounds a dead peer: headers within 30s (for a streamed
// request, see byStream), and an HTTP/2 connection that answers no ping for
// 30s+15s is dropped. A live peer that keeps silent is the body watch's.
func upstreamTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		HTTP2:                 &http.HTTP2Config{SendPingTimeout: 30 * time.Second, PingTimeout: 15 * time.Second},
	}
}

type streamKey struct{}

// markStream records on a request's context that the upstream answers it as a
// stream. Only such a request is held to the bounds on a silent upstream: a
// whole answer may send its headers or first byte only when it is done.
func markStream(ctx context.Context, stream bool) context.Context {
	if !stream {
		return ctx
	}
	return context.WithValue(ctx, streamKey{}, true)
}

func streamed(ctx context.Context) bool {
	marked, _ := ctx.Value(streamKey{}).(bool)
	return marked
}

// byStream sends a streamed request through stream and any other through a
// copy of it that waits for the headers as long as the upstream needs.
func byStream(stream *http.Transport) http.RoundTripper {
	whole := stream.Clone()
	whole.ResponseHeaderTimeout = 0
	return streamOrWhole{stream: stream, whole: whole}
}

type streamOrWhole struct{ stream, whole http.RoundTripper }

func (t streamOrWhole) RoundTrip(r *http.Request) (*http.Response, error) {
	if streamed(r.Context()) {
		return t.stream.RoundTrip(r)
	}
	return t.whole.RoundTrip(r)
}
