package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"localrouter/internal/privacy"
)

func (s *configStore) privacyRuntime() *privacy.Runtime {
	s.privacyOnce.Do(func() {
		home := ""
		if s.provPath != "" {
			home = filepath.Dir(s.provPath)
		}
		s.privacy = privacy.NewRuntime(home)
	})
	return s.privacy
}

type privacyContextKey struct{}
type privacyAttempt struct {
	policy   *privacy.Policy
	pool     string
	exchange *privacy.Exchange
}

func privateAttempt(r *http.Request) *privacyAttempt {
	v, _ := r.Context().Value(privacyContextKey{}).(*privacyAttempt)
	return v
}
func (p *privacyAttempt) prepare(target privacy.Target, body []byte) ([]byte, error) {
	p.exchange.Close()
	p.exchange = nil
	x, masked, err := p.policy.Prepare(target, body)
	p.exchange = x
	return masked, err
}

// privacyTraffic runs before any raw body/history capture. The legacy handler
// remains intact for explicit global-off; malformed config never reaches it.
func privacyTraffic(next http.Handler, cs *configStore, hl *health) http.Handler {
	runtime := cs.privacyRuntime()
	slots := make(chan struct{}, 4)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This endpoint is local management, never a forwarding route.
		parts := strings.Split(r.URL.Path, "/")
		if r.Method == http.MethodPost && len(parts) == 5 && parts[1] == "api" && parts[2] == "profiles" && parts[4] == "activate" {
			next.ServeHTTP(w, r)
			return
		}
		policy, err := runtime.Snapshot()
		reject := func(status int, message string) {
			runtime.Reject()
			w.Header().Set("Cache-Control", "no-store")
			writeAnthropicError(w, status, "invalid_request_error", message)
		}
		if err != nil {
			reject(503, "privacy: configuration unavailable or invalid; request was not sent")
			return
		}
		if !policy.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost || (r.URL.Path != "/v1/messages" && r.URL.Path != "/v1/messages/count_tokens") || r.URL.RawQuery != "" || r.URL.RawPath != "" {
			reject(400, "privacy: unsupported endpoint, method or query; request was not sent")
			return
		}
		typ, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || typ != "application/json" || r.Header.Get("Content-Encoding") != "" {
			reject(415, "privacy: only uncompressed application/json is supported")
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			reject(503, "privacy: capacity reached; retry later")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, privacy.TrafficInputLimit+1))
		if err != nil || len(body) > privacy.TrafficInputLimit || r.Context().Err() != nil {
			reject(400, "privacy: invalid, cancelled or oversized request")
			return
		}
		cfg := cs.get()
		model, route, err := configuredRequestRoute(cfg, body)
		if err != nil {
			reject(400, "privacy: invalid request or unavailable route")
			return
		}
		attempt := &privacyAttempt{policy: policy}
		defer func() { attempt.exchange.Close() }()
		r = r.WithContext(context.WithValue(r.Context(), privacyContextKey{}, attempt))
		output := &privacyBuffer{header: make(http.Header)}
		var raw []byte
		stream := false
		if route.Mode == "anthropic" {
			raw, err = attempt.prepare(privacy.Target{Model: model, Provider: "anthropic"}, body)
			if err == nil {
				err = privacyCloud(output, r, cfg, raw)
			}
		} else if r.URL.Path == "/v1/messages/count_tokens" {
			// This route has no upstream token endpoint, and sends no user content.
			output.Header().Set("Content-Type", "application/json")
			_, err = output.Write([]byte(`{"input_tokens":` + strconv.Itoa(len(body)/4) + `}`))
		} else {
			var probe anthropicRequest
			err = json.Unmarshal(body, &probe)
			if err == nil {
				localCfg := cfg.forModel(model, probe.OutputConfig.Effort)
				if localCfg.poolName != "" {
					attempt.pool = cfg.local.ActiveProfile + "/" + localCfg.poolName
				}
				handleLocal(output, r, localCfg, body, nil, hl)
			}
		}
		if output.status >= 300 || output.err != nil || r.Context().Err() != nil {
			err = errors.New("upstream failed")
		}
		if err != nil {
			reject(502, "privacy: request or upstream response rejected; no response content released")
			return
		}
		raw = output.body.Bytes()
		if r.URL.Path == "/v1/messages/count_tokens" {
			var values map[string]int
			if privacy.ValidateObject(raw) != nil || json.Unmarshal(raw, &values) != nil || len(values) != 1 || values["input_tokens"] < 0 {
				reject(502, "privacy: invalid token count response")
				return
			}
			if _, ok := values["input_tokens"]; !ok {
				reject(502, "privacy: invalid token count response")
				return
			}
		}
		stream = strings.HasPrefix(output.Header().Get("Content-Type"), "text/event-stream")
		if attempt.exchange != nil && r.URL.Path != "/v1/messages/count_tokens" {
			raw, err = attempt.exchange.Restore(raw, stream)
			if err != nil {
				reject(502, "privacy: response rejected; copy exact pseudonyms from the current context and retry without changing or encoding them")
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	})
}

type privacyBuffer struct {
	header http.Header
	body   bytes.Buffer
	status int
	err    error
}

func (w *privacyBuffer) Header() http.Header { return w.header }
func (w *privacyBuffer) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *privacyBuffer) Flush() {}
func (w *privacyBuffer) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if w.err != nil {
		return 0, w.err
	}
	if w.body.Len()+len(b) > privacy.TrafficOutputLimit {
		w.err = errors.New("privacy response limit")
		return 0, w.err
	}
	return w.body.Write(b)
}
func privacyCloud(w *privacyBuffer, r *http.Request, cfg config, body []byte) error {
	endpoint := *cfg.upstream
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + r.URL.Path
	endpoint.RawPath = ""
	endpoint.RawQuery = ""
	endpoint.Fragment = ""
	up, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	for _, key := range []string{"Authorization", "X-Api-Key", "Anthropic-Version", "Anthropic-Beta"} {
		for _, v := range r.Header.Values(key) {
			if key == "Anthropic-Version" || key == "Anthropic-Beta" {
				if err := privateAttempt(r).exchange.CheckControl(v); err != nil {
					return err
				}
			}
			up.Header.Add(key, v)
		}
	}
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Accept-Encoding", "identity")
	up.Header.Set("User-Agent", "localrouter")
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(up)
	if err != nil {
		return errors.New("privacy upstream unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.Header.Get("Content-Encoding") != "" {
		return errors.New("privacy upstream rejected")
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (media != "application/json" && media != "text/event-stream") {
		return errors.New("privacy unsupported response")
	}
	w.Header().Set("Content-Type", media)
	b, err := io.ReadAll(io.LimitReader(resp.Body, privacy.TrafficOutputLimit+1))
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// Unlike LimitReader, exhaustion is an error. EOF could otherwise make a
// truncated response (including trailing whitespace) look valid to a decoder.
type privacyResponseReader struct {
	source    io.Reader
	remaining int64
}

func (r *privacyResponseReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		var probe [1]byte
		n, err := r.source.Read(probe[:])
		if n > 0 {
			return 0, errors.New("privacy upstream response limit")
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.source.Read(p)
	r.remaining -= int64(n)
	return n, err
}
