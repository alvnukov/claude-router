package privacy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"localrouter/internal/anthropicerror"
)

type HTTPRoute struct {
	Mode, Model string
	Upstream    *url.URL
	Pool        func(string) (string, string)
	Local       func(http.ResponseWriter, *http.Request, []byte, string)
}

type HTTPDeps struct {
	Runtime    *Runtime
	Legacy     http.Handler
	Resolve    func([]byte) (HTTPRoute, error)
	Client     *http.Client
	Clock      LifecycleClock
	Limits     LifecycleLimits
	WriteError func(http.ResponseWriter, int, string, string)
	// ObserveAnthropicResponse reads subscription headers without capturing
	// bodies or changing the response. Observation must never fail a request.
	ObserveAnthropicResponse func(*http.Response) error
}

type LifecycleClock interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) LifecycleTimer
}

type LifecycleTimer interface{ Stop() bool }

type LifecycleLimits struct {
	Inbound, Headers, Idle, Total, Cleanup time.Duration
}

func NewProtectedHTTP(deps HTTPDeps) http.Handler {
	limits := deps.Limits
	if limits == (LifecycleLimits{}) {
		limits = LifecycleLimits{Inbound: 120 * time.Second, Headers: 120 * time.Second, Idle: 90 * time.Second, Total: 900 * time.Second, Cleanup: 3 * time.Second}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/profiles/") && strings.HasSuffix(r.URL.Path, "/activate") {
			if deps.Legacy != nil {
				deps.Legacy.ServeHTTP(w, r)
			}
			return
		}
		reject := func(status int, kind, message string) {
			if deps.Runtime != nil {
				deps.Runtime.Reject()
			}
			for key := range w.Header() {
				if strings.EqualFold(key, "Retry-After") || strings.EqualFold(key, "Content-Length") {
					delete(w.Header(), key)
				}
			}
			w.Header().Set("Cache-Control", "no-store")
			if deps.WriteError != nil {
				deps.WriteError(w, status, kind, message)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": message}})
		}
		if deps.Runtime == nil {
			reject(503, "api_error", "privacy: configuration unavailable")
			return
		}
		policy, err := deps.Runtime.Snapshot()
		if err != nil {
			reject(503, "api_error", "privacy: configuration unavailable")
			return
		}
		if !policy.Enabled() {
			if deps.Legacy == nil {
				reject(503, "api_error", "privacy: route unavailable")
				return
			}
			deps.Legacy.ServeHTTP(w, r)
			return
		}
		if policy.observationOnly() {
			serveObservedHTTP(w, r, policy, deps)
			return
		}
		if r.Method != http.MethodPost || r.URL.RawPath != "" || (r.URL.Path != "/v1/messages" && r.URL.Path != "/v1/messages/count_tokens") || !supportedPrivacyQuery(r.URL.RawQuery) {
			reject(400, "invalid_request_error", "privacy: unsupported endpoint or query")
			return
		}
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" || r.Header.Get("Content-Encoding") != "" {
			reject(415, "invalid_request_error", "privacy: unsupported request content type")
			return
		}
		if !limits.valid() || deps.Resolve == nil {
			reject(503, "api_error", "privacy: configuration unavailable")
			return
		}
		controller := http.NewResponseController(w)
		if controller.SetReadDeadline(time.Now().Add(limits.Inbound)) != nil || controller.SetWriteDeadline(time.Now().Add(limits.Inbound+limits.Total)) != nil {
			reject(503, "api_error", "privacy: client transport cannot be bounded")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, TrafficInputLimit+1))
		if err != nil || len(body) > TrafficInputLimit || r.Context().Err() != nil || ValidateObject(body) != nil {
			reject(400, "invalid_request_error", "privacy: invalid or incomplete request")
			return
		}
		_ = controller.SetReadDeadline(time.Time{})
		route, err := deps.Resolve(body)
		if err != nil {
			reject(400, "invalid_request_error", "privacy: route unavailable")
			return
		}
		if route.Mode == "anthropic" {
			serveProtectedDirect(w, r, body, route, policy, deps, limits, controller, reject)
			return
		}
		if route.Mode == "model" || route.Mode == "pool" || route.Mode == "openai" || route.Mode == "codex" {
			serveProtectedLocal(w, r, body, route, policy, deps, limits, controller, reject)
			return
		}
		reject(503, "api_error", "privacy: route unavailable")
	})
}

type protectedLocalBuffer struct {
	header    http.Header
	body      bytes.Buffer
	life      *protectedLifecycle
	open      map[string]string
	processed int
	terminal  bool
	status    int
	err       error
}

func (b *protectedLocalBuffer) Header() http.Header { return b.header }
func (b *protectedLocalBuffer) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
		b.life.gotHeaders(nil)
	}
}
func (b *protectedLocalBuffer) Flush() {}
func (b *protectedLocalBuffer) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.WriteHeader(http.StatusOK)
	}
	if b.err != nil {
		return 0, b.err
	}
	if b.life.expired.Load() {
		b.err = errors.New("privacy: response limit or deadline")
		return 0, b.err
	}
	media, _, _ := mime.ParseMediaType(b.header.Get("Content-Type"))
	if media == "text/event-stream" && b.terminal {
		return len(p), nil
	}
	inputLen := len(p)
	remaining := TrafficOutputLimit - b.body.Len()
	overLimit := len(p) > remaining
	if overLimit {
		p = p[:remaining]
	}
	n, err := b.body.Write(p)
	if media == "text/event-stream" {
		if b.open == nil {
			b.open = make(map[string]string)
		}
		for {
			frame, end, complete := nextProtectedFrame(b.body.Bytes(), b.processed)
			if !complete {
				break
			}
			b.processed = end
			if observeProtectedFrame(frame, b.open, b.life) {
				b.terminal = true
				b.body.Truncate(end)
				return inputLen, nil
			}
		}
	} else if media == "application/json" && n > 0 {
		b.life.useful()
	}
	if overLimit {
		b.err = errors.New("privacy: response limit or deadline")
		return 0, b.err
	}
	return n, err
}

func serveProtectedLocal(w http.ResponseWriter, r *http.Request, body []byte, route HTTPRoute, policy *Policy, deps HTTPDeps, limits LifecycleLimits, controller *http.ResponseController, reject func(int, string, string)) {
	if route.Local == nil || r.Context().Err() != nil {
		reject(503, "api_error", "privacy: local route unavailable")
		return
	}
	attempt := NewAttempt(policy)
	defer attempt.Close()
	effort, _ := lookupString(body, "output_config", "effort")
	if route.Pool != nil {
		profile, pool := route.Pool(effort)
		attempt.SetPool(profile, pool)
	}
	parent := r.Context()
	ctx, cancel := context.WithCancel(parent)
	r = WithAttempt(r.WithContext(ctx), attempt)
	clock := deps.Clock
	if clock == nil {
		clock = wallLifecycleClock{}
	}
	life := &protectedLifecycle{clock: clock, limits: limits, controller: controller}
	if err := controller.SetWriteDeadline(time.Now().Add(limits.Total)); err != nil {
		cancel()
		reject(503, "api_error", "privacy: client transport cannot be bounded")
		return
	}
	attempt.lifecycle = life
	life.begin(parent, cancel)
	defer life.stop()
	output := &protectedLocalBuffer{header: make(http.Header), life: life}
	route.Local(output, r, body, effort)
	if life.expired.Load() || r.Context().Err() != nil || output.err != nil {
		reject(502, "api_error", "privacy: upstream response rejected")
		return
	}
	life.headers.Stop()
	if attempt.rejected {
		reject(400, "invalid_request_error", "privacy: request rejected by selected profile")
		return
	}
	if output.status < 200 || output.status >= 300 {
		failure := FailureForResponse(r, nil, output.status)
		if (route.Mode == "codex" || attempt.Protocol() == "codex") && failure.Status == http.StatusTooManyRequests && failure.HTTPStatus == http.StatusTooManyRequests && failure.Category != anthropicerror.UpstreamFailure {
			anthropicerror.WriteHTTP(w, failure)
			return
		}
		reject(502, "api_error", "privacy: upstream response rejected")
		return
	}
	if attempt.Failed() {
		reject(502, "api_error", "privacy: upstream response rejected")
		return
	}
	mode := policy.config.Resolve(Target{Model: route.Model})
	count := r.URL.Path == "/v1/messages/count_tokens"
	if count && !attempt.Prepared() {
		reject(400, "invalid_request_error", "privacy: unverified token count")
		return
	}
	if !attempt.Prepared() && mode.Enabled && mode.Mode == ModeMask || attempt.MaskExpected() && !attempt.HasExchange() {
		reject(502, "api_error", "privacy: unverified local response")
		return
	}
	media, _, err := mime.ParseMediaType(output.Header().Get("Content-Type"))
	if err != nil || (media != "application/json" && media != "text/event-stream") {
		reject(502, "api_error", "privacy: unsupported local response")
		return
	}
	stream := media == "text/event-stream"
	result := output.body.Bytes()
	if count && (stream || !validTokenCount(result)) {
		reject(502, "api_error", "privacy: invalid token count response")
		return
	}
	if stream {
		result, err = readProtectedBody(bytes.NewReader(result), true, life)
		if err == nil && !attempt.HasExchange() {
			err = checkUnmaskedTrafficSSE(result)
		}
	}
	if err == nil && attempt.HasExchange() {
		result, err = attempt.Restore(result, stream)
	} else if err == nil && !stream {
		err = ValidateObject(result)
	}
	if err != nil || life.expired.Load() || r.Context().Err() != nil {
		reject(502, "api_error", "privacy: upstream response rejected")
		return
	}
	if !life.commit(r.Context()) {
		reject(502, "api_error", "privacy: upstream response rejected")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", media)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result)
}

func serveProtectedDirect(w http.ResponseWriter, r *http.Request, body []byte, route HTTPRoute, policy *Policy, deps HTTPDeps, limits LifecycleLimits, controller *http.ResponseController, reject func(int, string, string)) {
	badInput := func() { reject(400, "invalid_request_error", "privacy: invalid protocol headers") }
	// The *.anthropic.com allowlist is checked once at startup (config.CheckUpstream); a new source of Upstream must check it too.
	if route.Upstream == nil || (route.Upstream.Scheme != "https" && route.Upstream.Scheme != "http") || route.Upstream.Host == "" || route.Upstream.User != nil || route.Upstream.Opaque != "" {
		reject(503, "api_error", "privacy: upstream not configured for protected transport")
		return
	}
	apiKeys, authorizations := r.Header.Values("X-Api-Key"), r.Header.Values("Authorization")
	if len(apiKeys)+len(authorizations) != 1 {
		badInput()
		return
	}
	authName, authValue := "X-Api-Key", ""
	if len(apiKeys) == 1 {
		authValue = apiKeys[0]
	} else {
		authName, authValue = "Authorization", authorizations[0]
	}
	if authValue == "" || strings.TrimSpace(authValue) != authValue || strings.ContainsAny(authValue, "\r\n") {
		badInput()
		return
	}
	beta, version := r.Header.Values("Anthropic-Beta"), r.Header.Values("Anthropic-Version")
	if len(version) > 1 {
		badInput()
		return
	}
	versionValue := "2023-06-01"
	if len(version) == 1 {
		versionValue = version[0]
	}
	if versionValue != "2023-06-01" {
		badInput()
		return
	}
	target := Target{Model: route.Model, Provider: "anthropic", Betas: beta, TokenCount: r.URL.Path == "/v1/messages/count_tokens"}
	// Prepare masks supported content according to the resolved profile.
	// Client controls and opaque content do not prevent preparation.
	exchange, wire, err := policy.Prepare(target, DropUnsignedThinking(body))
	if err != nil {
		reject(400, "invalid_request_error", "privacy: request preparation failed")
		return
	}
	if exchange != nil {
		defer exchange.Close()
		if err := exchange.CheckControl(versionValue); err != nil {
			badInput()
			return
		}
		for _, value := range beta {
			if err := exchange.CheckControl(value); err != nil {
				badInput()
				return
			}
		}
	}
	endpoint := *route.Upstream
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + r.URL.Path
	endpoint.RawPath, endpoint.RawQuery, endpoint.Fragment = "", r.URL.RawQuery, ""
	parent := r.Context()
	ctx, cancel := context.WithCancel(parent)
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(wire))
	if err != nil {
		cancel()
		reject(503, "api_error", "privacy: upstream not configured for protected transport")
		return
	}
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Accept-Encoding", "identity")
	up.Header.Set("User-Agent", "localrouter")
	up.Header.Set(authName, authValue)
	up.Header.Set("Anthropic-Version", versionValue)
	// Provider protocol controls are opaque to content masking. Preserve new
	// SDK headers as well as beta/version without forwarding unrelated headers.
	for key, values := range r.Header {
		if strings.HasPrefix(strings.ToLower(key), "anthropic-") {
			up.Header[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
		}
	}
	client := deps.Client
	if client == nil {
		client = &http.Client{Transport: &http.Transport{Proxy: nil}}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	clock := deps.Clock
	if clock == nil {
		clock = wallLifecycleClock{}
	}
	life := &protectedLifecycle{clock: clock, limits: limits, controller: controller}
	if err := controller.SetWriteDeadline(time.Now().Add(limits.Total)); err != nil {
		cancel()
		reject(503, "api_error", "privacy: client transport cannot be bounded")
		return
	}
	life.begin(parent, cancel)
	defer life.stop()
	resp, err := copyClient.Do(up)
	if err != nil || resp == nil {
		reject(502, "api_error", "privacy: upstream response rejected")
		return
	}
	if resp.Body == nil {
		reject(502, "api_error", "privacy: upstream response rejected")
		return
	}
	life.gotHeaders(resp.Body)
	if deps.ObserveAnthropicResponse != nil {
		_ = deps.ObserveAnthropicResponse(resp)
	}
	if resp.StatusCode >= 400 && resp.StatusCode <= 599 {
		if !life.commit(r.Context()) {
			reject(502, "api_error", "privacy: upstream response rejected")
			return
		}
		writeProtectedProviderError(w, deps, resp)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.Header.Get("Content-Encoding") != "" {
		reject(502, "api_error", "privacy: upstream response rejected")
		return
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (media != "application/json" && media != "text/event-stream") {
		reject(502, "api_error", "privacy: upstream response rejected")
		return
	}
	stream := media == "text/event-stream"
	if target.TokenCount && stream {
		reject(502, "api_error", "privacy: invalid token count response")
		return
	}
	result, err := readProtectedBody(resp.Body, stream, life)
	if err != nil || r.Context().Err() != nil || life.expired.Load() {
		reject(502, "api_error", "privacy: upstream response rejected")
		return
	}
	if exchange != nil {
		result, err = exchange.Restore(result, stream)
	} else if stream {
		err = checkUnmaskedTrafficSSE(result)
	} else {
		err = ValidateObject(result)
	}
	if err == nil && target.TokenCount && !validTokenCount(result) {
		err = errTraffic
	}
	if err != nil || life.expired.Load() || r.Context().Err() != nil {
		reject(502, "api_error", "privacy: upstream response rejected")
		return
	}
	if !life.commit(r.Context()) {
		reject(502, "api_error", "privacy: upstream response rejected")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", media)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result)
}

func supportedPrivacyQuery(raw string) bool {
	if raw == "" {
		return true
	}
	values, err := url.ParseQuery(raw)
	return err == nil && len(values) == 1 && len(values["beta"]) == 1 && values.Get("beta") == "true"
}

// Preserve provider status semantics without exposing its response body.
func writeProtectedProviderError(w http.ResponseWriter, deps HTTPDeps, resp *http.Response) {
	if deps.Runtime != nil {
		deps.Runtime.Reject()
	}
	for key := range w.Header() {
		if strings.EqualFold(key, "Retry-After") || strings.EqualFold(key, "Content-Length") {
			delete(w.Header(), key)
		}
	}
	if resp.StatusCode == 429 || resp.StatusCode == 503 || resp.StatusCode == 529 {
		if values := resp.Header.Values("Retry-After"); len(values) == 1 {
			value := strings.TrimSpace(values[0])
			if seconds, err := strconv.ParseUint(value, 10, 32); err == nil && seconds <= 86400 {
				w.Header().Set("Retry-After", strconv.FormatUint(seconds, 10))
			} else if date, err := http.ParseTime(value); err == nil && time.Until(date) >= 0 && time.Until(date) <= 24*time.Hour {
				w.Header().Set("Retry-After", date.UTC().Format(http.TimeFormat))
			}
		}
	}
	kind := map[int]string{400: "invalid_request_error", 401: "authentication_error", 403: "permission_error", 404: "not_found_error", 413: "request_too_large", 429: "rate_limit_error", 529: "overloaded_error"}[resp.StatusCode]
	if kind == "" {
		kind = "api_error"
	}
	w.Header().Set("Cache-Control", "no-store")
	const message = "The upstream service rejected the request."
	if deps.WriteError != nil {
		deps.WriteError(w, resp.StatusCode, kind, message)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": message}})
}

func validTokenCount(body []byte) bool {
	n, err := scanJSON(body)
	if err != nil || n.kind != '{' {
		return false
	}
	count := n.get("input_tokens")
	if count == nil {
		return false
	}
	value, err := strconv.ParseInt(string(body[count.start:count.end]), 10, 64)
	return err == nil && value >= 0
}

func (limits LifecycleLimits) valid() bool {
	return limits.Inbound > 0 && limits.Inbound <= 24*time.Hour && limits.Headers > 0 && limits.Idle > 0 && limits.Total >= limits.Headers && limits.Total >= limits.Idle && limits.Total <= 24*time.Hour && limits.Cleanup > 0 && limits.Cleanup <= 3*time.Second
}
