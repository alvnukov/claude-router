package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	conf "localrouter/internal/config"
	webui "localrouter/internal/ui"
)

func TestDefaultPoolRoutingPrecedence(t *testing.T) {
	l := localSetup{DefaultPool: "fallback", ModelPools: map[string][]poolTarget{"fallback": {{Model: "p/m", Effort: conf.PoolRequestEffort}}}, Models: []localModel{{Provider: "p", Model: "m"}}, Routes: map[string]map[string]modelRoute{"explicit-off": {"default": {Mode: "disabled"}}, "version": {"high": {Mode: "anthropic"}}}, FamilyRoutes: map[string]map[string]modelRoute{"sonnet": {"high": {Mode: "anthropic"}, "low": {Mode: "disabled"}}}}
	for _, tc := range []struct{ model, effort, mode string }{
		{"future-model", "high", "pool"}, {"future-model", "", "pool"}, {"future-model", "invalid", "disabled"}, {"", "high", "disabled"},
		{"explicit-off", "default", "disabled"}, {"explicit-off", "high", "disabled"}, {"version", "high", "anthropic"}, {"version", "low", "disabled"},
		{"claude-sonnet-99", "high", "anthropic"}, {"claude-sonnet-99", "low", "disabled"}, {"claude-sonnet-99", "medium", "disabled"},
	} {
		if got := l.RouteFor(tc.model, tc.effort); got.Mode != tc.mode {
			t.Fatalf("%s/%s: %+v", tc.model, tc.effort, got)
		}
	}
	if got := (config{Local: l}).ForModel("future-model", "high").Local.Models[0].Efforts["high"]; got != "high" {
		t.Fatal("fallback lost effort policy")
	}
	l.DefaultPool = ""
	if l.RouteFor("future-model", "high").Mode != "disabled" {
		t.Fatal("disabled fallback still routes")
	}
}

func TestUIJSONDefaultPoolPersistenceAndHistory(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openaiRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.ReasoningEffort != "high" {
			t.Errorf("effort=%q", req.ReasoningEffort)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"fallback ok"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()
	u, h := testUI(t)
	u.cs = conf.NewStore(u.cs.Get(), filepath.Join(t.TempDir(), "providers.json"))
	l := u.cs.Get().Local.Clone()
	l.Providers[0].BaseURL = up.URL
	l.ModelPools = map[string][]poolTarget{"fallback": {{Model: "p/m1", Effort: conf.PoolRequestEffort}}}
	l.ActiveProfile = "active"
	l.Profiles = map[string]conf.Profile{"active": l.Routing(), "other": l.Routing()}
	c := u.cs.Get()
	c.Local = l
	u.cs = conf.NewStore(c, u.cs.Path())
	set := func(profile, name string) int {
		return apiCall(t, h, "POST", "/api/ui/actions", webui.Action{Action: "pool.edit", Fields: map[string]string{"profile": profile, "op": "default", "name": name}}).Code
	}
	if set("active", "missing") != 400 || set("other", "fallback") != 400 || set("active", "fallback") != 200 {
		t.Fatal("default pool validation")
	}
	reloaded, err := conf.ReadProviders(u.cs.Path())
	if err != nil || reloaded.DefaultPool != "fallback" || reloaded.Profiles["other"].DefaultPool != "" {
		t.Fatalf("persisted default: %+v %v", reloaded, err)
	}
	if err := u.cs.CreateProfile("copy", true); err != nil {
		t.Fatal(err)
	}
	if u.cs.Get().Local.Profiles["copy"].DefaultPool != "fallback" {
		t.Fatal("profile clone lost default")
	}
	w := apiCall(t, h, "POST", "/api/ui/actions", webui.Action{Action: "pool.edit", Fields: map[string]string{"profile": "active", "op": "delete", "name": "fallback"}})
	if w.Code != 400 {
		t.Fatal("default pool deletion accepted")
	}
	upURL, _ := url.Parse(up.URL)
	cfg := u.cs.Get()
	cfg.Upstream = upURL
	proxy := newMainHandler(cfg, u.cs, u.st, u.hl, u)
	w = httptest.NewRecorder()
	proxy.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"unknown-debug-model","output_config":{"effort":"high"},"messages":[{"role":"user","content":"hi"}]}`)))
	if w.Code != 200 {
		t.Fatalf("fallback request: %d %s", w.Code, w.Body.String())
	}
	rec := u.st.List()[0]
	if rec.FallbackPool != "fallback" || rec.UnrecognizedReason == "" || rec.Served != "p/m1" {
		t.Fatalf("missing fallback explanation: %+v", (uiBackend{u}).requestDTO(rec))
	}
	if (uiBackend{u}).Requests(url.Values{"unrecognized": {"1"}}).Total != 1 {
		t.Fatal("fallback absent in diagnostics")
	}
	if set("active", "") != 200 || u.cs.Get().Local.RouteFor("unknown-debug-model", "high").Mode != "disabled" {
		t.Fatal("fallback not disabled")
	}
}
