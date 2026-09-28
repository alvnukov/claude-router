package privacy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// An observational transport is safe only when no selected provider or failover
// candidate can require masking. Mixed policies retain the protected transport.
func (p *Policy) observationOnly() bool {
	if !p.Enabled() {
		return false
	}
	detect := false
	for _, profile := range p.config.Profiles {
		if !profile.Enabled {
			continue
		}
		if effectiveMode(profile.Mode) != ModeDetect {
			return false
		}
		detect = true
	}
	return detect
}

// Detection sees request content, never owns the provider response, and never
// invokes Legacy (which may record raw request/response history).
func serveObservedHTTP(w http.ResponseWriter, r *http.Request, policy *Policy, deps HTTPDeps) {
	if deps.Resolve == nil {
		observedError(w, deps, http.StatusServiceUnavailable, "router configuration unavailable")
		return
	}
	if r.URL.Path != "/v1/messages" && r.URL.Path != "/v1/messages/count_tokens" {
		route, _ := deps.Resolve(nil)
		serveObservedProxy(w, r, route.Upstream, deps)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		observedError(w, deps, http.StatusBadRequest, "could not read request body")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	route, err := deps.Resolve(body)
	if err != nil {
		observedError(w, deps, http.StatusBadRequest, "configured route unavailable")
		return
	}
	if route.Mode == "anthropic" {
		_, _, _ = policy.Prepare(Target{Model: route.Model, Provider: "anthropic"}, body)
		serveObservedProxy(w, r, route.Upstream, deps)
		return
	}
	if route.Local == nil {
		observedError(w, deps, http.StatusBadGateway, "configured provider unavailable")
		return
	}
	attempt := NewAttempt(policy)
	defer attempt.Close()
	effort, _ := lookupString(body, "output_config", "effort")
	if route.Pool != nil {
		profile, pool := route.Pool(effort)
		attempt.SetPool(profile, pool)
	}
	route.Local(w, WithAttempt(r, attempt), body, effort)
}

var observedTransport = &http.Transport{Proxy: nil}

func serveObservedProxy(w http.ResponseWriter, r *http.Request, upstream *url.URL, deps HTTPDeps) {
	if upstream == nil {
		observedError(w, deps, http.StatusBadGateway, "upstream unavailable")
		return
	}
	var transport http.RoundTripper = observedTransport
	if deps.Client != nil && deps.Client.Transport != nil {
		transport = deps.Client.Transport
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(upstream)
			p.Out.Header = make(http.Header)
			for key, values := range p.In.Header {
				name := strings.ToLower(key)
				if name == "authorization" || name == "x-api-key" || name == "content-type" || name == "content-encoding" || strings.HasPrefix(name, "anthropic-") {
					p.Out.Header[key] = append([]string(nil), values...)
				}
			}
			p.Out.Header.Set("User-Agent", "localrouter")
		},
		Transport:     transport,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if deps.ObserveAnthropicResponse != nil {
				_ = deps.ObserveAnthropicResponse(resp)
			}
			for key := range resp.Header {
				name := strings.ToLower(key)
				if name != "content-type" && name != "content-encoding" && name != "content-length" && name != "retry-after" && name != "request-id" && name != "location" && name != "www-authenticate" && !strings.HasPrefix(name, "anthropic-") {
					delete(resp.Header, key)
				}
			}
			resp.Header.Set("Cache-Control", "no-store")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			observedError(w, deps, http.StatusBadGateway, "upstream unreachable")
		},
	}
	proxy.ServeHTTP(w, r)
}

func observedError(w http.ResponseWriter, deps HTTPDeps, status int, message string) {
	if deps.WriteError != nil {
		deps.WriteError(w, status, "api_error", message)
		return
	}
	http.Error(w, message, status)
}
