package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsRejectCrossOriginMutations(t *testing.T) {
	u, h := testUI(t)
	u.cs = newConfigStore(u.cs.get(), filepath.Join(t.TempDir(), "providers.json"))
	l := u.cs.get().local.clone()
	l.ModelPools = map[string][]poolTarget{"work": {{Model: "p/m1"}}}
	l.Routes = map[string]map[string]modelRoute{"claude-opus-5": {"high": {Mode: "anthropic"}}}
	if err := u.cs.applyLocal(l, false); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path string
		form url.Values
	}{
		{"/settings/route", url.Values{"model": {"claude-opus-5"}, "high": {"pool:work"}}},
		{"/settings/pools", url.Values{"op": {"delete"}, "name": {"work"}}},
		{"/settings/providers", url.Values{"op": {"remove"}, "name": {"p"}}},
		{"/settings/models", url.Values{"op": {"remove"}, "key": {"p/m1"}}},
		{"/settings/pool-settings", url.Values{"name": {"work"}, "failover": {"0"}}},
		{"/requests/clear", url.Values{}},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://localhost:8788"+tc.path, strings.NewReader(tc.form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", "https://attacker.example")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("cross-origin POST: got %d, want 403", w.Code)
			}
		})
	}
	cfg := u.cs.get()
	if cfg.routeFor("claude-opus-5", "high").Mode != "anthropic" || len(cfg.local.ModelPools["work"]) != 1 || len(cfg.local.Providers) != 1 || len(cfg.local.Models) != 1 {
		t.Fatalf("cross-origin POST changed settings: %+v", cfg.local)
	}
	r := httptest.NewRequest(http.MethodPost, "http://localhost:8788/settings/route", strings.NewReader(url.Values{"model": {"claude-opus-5"}, "high": {"pool:work"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://localhost:8788")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || u.cs.get().routeFor("claude-opus-5", "high").Pool != "work" {
		t.Fatalf("same-origin update rejected: status=%d, route=%+v", w.Code, u.cs.get().routeFor("claude-opus-5", "high"))
	}
}
