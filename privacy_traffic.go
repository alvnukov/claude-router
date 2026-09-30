package main

import (
	"bytes"
	"errors"
	"net/http"

	"localrouter/internal/privacy"
)

// privacyTraffic runs before any raw body/history capture. The legacy handler
// remains intact for explicit global-off; malformed config never reaches it.
func privacyTraffic(next http.Handler, cs *configStore, hl *health, observeAnthropic func(*http.Response) error) http.Handler {
	return privacy.NewProtectedHTTP(privacy.HTTPDeps{
		Runtime: cs.PrivacyRuntime(), Legacy: next, WriteError: writeAnthropicError,
		ObserveAnthropicResponse: observeAnthropic,
		Resolve: func(body []byte) (privacy.HTTPRoute, error) {
			cfg := cs.Get()
			model, route, err := configuredRequestRoute(cfg, body)
			return privacy.HTTPRoute{
				Mode: route.Mode, Model: model, Upstream: cfg.Upstream,
				Pool: func(effort string) (string, string) {
					return cfg.Local.ActiveProfile, cfg.ForModel(model, effort).PoolName
				},
				Local: func(w http.ResponseWriter, r *http.Request, b []byte, effort string) {
					handleLocal(w, r, cfg.ForModel(model, effort), b, nil, hl)
				},
			}, err
		},
	})
}

// The legacy decoder diagnostic test uses a bounded in-memory writer.
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
		w.status = http.StatusOK
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
