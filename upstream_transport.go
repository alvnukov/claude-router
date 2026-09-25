package main

import (
	"net"
	"net/http"
	"time"
)

// upstreamHTTP carries every request to Anthropic and Codex; the tests point
// it back at http.DefaultTransport, where they fake the network.
var upstreamHTTP http.RoundTripper = upstreamTransport()

// upstreamTransport bounds a dead peer: headers within 30s, and an HTTP/2
// connection that answers no ping for 30s+15s is dropped. A live peer that
// keeps silent is the body watch's.
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
