package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	conf "localrouter/internal/config"
)

func TestSettingsRejectCrossOriginMutations(t *testing.T) {
	u, h := testUI(t)
	u.cs = conf.NewStore(u.cs.Get(), filepath.Join(t.TempDir(), "providers.json"))
	l := u.cs.Get().Local.Clone()
	l.ModelPools = map[string][]poolTarget{"work": {{Model: "p/m1"}}}
	l.Routes = map[string]map[string]modelRoute{"claude-opus-5": {"high": {Mode: "anthropic"}}}
	if err := u.cs.Update(conf.Replace(l, "")); err != nil {
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
	cfg := u.cs.Get()
	if cfg.RouteFor("claude-opus-5", "high").Mode != "anthropic" || len(cfg.Local.ModelPools["work"]) != 1 || len(cfg.Local.Providers) != 1 || len(cfg.Local.Models) != 1 {
		t.Fatalf("cross-origin POST changed settings: %+v", cfg.Local)
	}
	r := httptest.NewRequest(http.MethodPost, "http://localhost:8788/settings/route", strings.NewReader(url.Values{"model": {"claude-opus-5"}, "high": {"pool:work"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://localhost:8788")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || u.cs.Get().RouteFor("claude-opus-5", "high").Pool != "work" {
		t.Fatalf("same-origin update rejected: status=%d, route=%+v", w.Code, u.cs.Get().RouteFor("claude-opus-5", "high"))
	}
}
