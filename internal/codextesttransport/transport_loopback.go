//go:build router_codex_loopback

package codextesttransport

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"time"
)

const (
	testToken   = "Bearer RR_SYNTHETIC.eyJleHAiOjQxMDI0NDQ4MDB9.sig"
	testAccount = "RR_SYNTHETIC_ACCOUNT"
)

type loopbackTransport struct{}

func Transport() http.RoundTripper { return loopbackTransport{} }

// AuthTransport rejects OAuth refresh: the synthetic credential must remain valid.
func AuthTransport() http.RoundTripper { return loopbackTransport{} }

func DisableBackground() bool { return true }

func (loopbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil || req.Method != http.MethodPost || req.URL.Scheme != "https" ||
		req.URL.Host != "chatgpt.com" || req.URL.EscapedPath() != "/backend-api/codex/responses" ||
		req.URL.User != nil || req.URL.RawQuery != "" || req.URL.ForceQuery || req.URL.Fragment != "" ||
		(req.Host != "" && req.Host != "chatgpt.com") ||
		len(req.Header.Values("Authorization")) != 1 || req.Header.Get("Authorization") != testToken ||
		len(req.Header.Values("ChatGPT-Account-Id")) != 1 || req.Header.Get("ChatGPT-Account-Id") != testAccount {
		return nil, errors.New("synthetic Codex transport rejected request")
	}
	stub, err := url.Parse(os.Getenv("ROUTER_CODEX_TEST_STUB_URL"))
	if err != nil || stub == nil || stub.Scheme != "http" || stub.User != nil || stub.Opaque != "" ||
		stub.Path != "" || stub.RawPath != "" || stub.RawQuery != "" || stub.ForceQuery || stub.Fragment != "" {
		return nil, errors.New("synthetic Codex transport requires an IP-loopback HTTP stub")
	}
	ip, err := netip.ParseAddr(stub.Hostname())
	if err != nil || !ip.IsLoopback() || ip.Zone() != "" {
		return nil, errors.New("synthetic Codex transport requires an IP-loopback HTTP stub")
	}
	port, err := strconv.Atoi(stub.Port())
	if err != nil || port < 1 || port > 65535 || stub.Host != net.JoinHostPort(ip.String(), strconv.Itoa(port)) {
		return nil, errors.New("synthetic Codex transport requires an IP-loopback HTTP stub")
	}
	mapped := req.Clone(req.Context())
	mappedURL := *req.URL
	mappedURL.Scheme, mappedURL.Host = "http", stub.Host
	mapped.URL, mapped.Host = &mappedURL, stub.Host
	mapped.Header = make(http.Header)
	for _, key := range []string{"Content-Type", "Accept", "OpenAI-Beta", "Authorization", "ChatGPT-Account-Id"} {
		if values := req.Header.Values(key); len(values) == 1 {
			mapped.Header.Set(key, values[0])
		}
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		Proxy: nil, DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr != stub.Host {
				return nil, errors.New("synthetic Codex transport rejected socket destination")
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	return transport.RoundTrip(mapped)
}
