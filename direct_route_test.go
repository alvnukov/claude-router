package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDirectRouteUsesOnlySelectedModelAndEffort(t *testing.T) {
	var otherCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model  string `json:"model"`
			Effort string `json:"reasoning_effort"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model == "other" {
			otherCalls.Add(1)
		}
		if body.Model != "chosen" || body.Effort != "xhigh" {
			t.Errorf("sent to %s with effort %s, want chosen/xhigh", body.Model, body.Effort)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":"unavailable"}`)
	}))
	defer server.Close()
	l := localSetup{
		Providers: []provider{{Name: "p", BaseURL: server.URL}},
		Models:    []localModel{{Provider: "p", Model: "other"}, {Provider: "p", Model: "chosen"}},
		FamilyRoutes: map[string]map[string]modelRoute{
			"opus": {"high": {Mode: "model", Model: "p/chosen", Effort: "xhigh"}},
		},
	}
	if err := l.validate(); err != nil {
		t.Fatal(err)
	}
	cfg := config{local: l, failover: true}
	body := []byte(`{"model":"claude-opus-5","output_config":{"effort":"high"},"messages":[{"role":"user","content":"hi"}]}`)
	_, route, err := configuredRequestRoute(cfg, body)
	if err != nil || route.Mode != "model" {
		t.Fatalf("direct route: %+v, %v", route, err)
	}
	w := httptest.NewRecorder()
	handleLocal(w, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), cfg.forModel("claude-opus-5", "high"), body, nil, newHealth(""))
	if w.Code != http.StatusServiceUnavailable || otherCalls.Load() != 0 {
		t.Fatalf("direct route fell through: status=%d other calls=%d body=%s", w.Code, otherCalls.Load(), w.Body.String())
	}
}

func TestDirectRouteRejectsInvalidTargets(t *testing.T) {
	base := localSetup{
		Providers: []provider{{Name: "p", BaseURL: "http://127.0.0.1:1234/v1"}},
		Models:    []localModel{{Provider: "p", Model: "chosen"}},
	}
	for _, tc := range []struct {
		name  string
		route modelRoute
	}{
		{"missing model", modelRoute{Mode: "model", Model: "p/missing", Effort: "high"}},
		{"invalid effort", modelRoute{Mode: "model", Model: "p/chosen", Effort: "invalid"}},
		{"pool on direct route", modelRoute{Mode: "model", Model: "p/chosen", Pool: "other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := base.clone()
			l.Routes = map[string]map[string]modelRoute{"claude-opus-5": {"high": tc.route}}
			if err := l.validate(); err == nil {
				t.Fatal("invalid direct route accepted")
			}
		})
	}
}

func TestDirectRouteProviderRenameAndModelRemoval(t *testing.T) {
	u, h := testUI(t)
	u.cs.provPath = filepath.Join(t.TempDir(), "providers.json")
	get(t, h, "POST", "/settings/route", url.Values{"model": {"opus"}, "scope": {"family"}, "high": {"model:p/m1:high"}})
	get(t, h, "POST", "/settings/providers", url.Values{"op": {"update"}, "orig": {"p"}, "name": {"renamed"}, "base_url": {u.cs.get().local.Providers[0].BaseURL}})
	got := u.cs.get().routeFor("claude-opus-5", "high")
	if got.Mode != "model" || got.Model != "renamed/m1" || got.Effort != "high" {
		t.Fatalf("provider rename broke direct route: %+v", got)
	}
	get(t, h, "POST", "/settings/models", url.Values{"op": {"remove"}, "key": {"renamed/m1"}})
	if got := u.cs.get().routeFor("claude-opus-5", "high"); got.Mode != "disabled" {
		t.Fatalf("removed model left direct route: %+v", got)
	}
}

func TestDirectRouteProviderRemovalDisablesAssignment(t *testing.T) {
	u, h := testUI(t)
	u.cs.provPath = filepath.Join(t.TempDir(), "providers.json")
	get(t, h, "POST", "/settings/route", url.Values{"model": {"opus"}, "scope": {"family"}, "high": {"model:p/m1:high"}})
	get(t, h, "POST", "/settings/providers", url.Values{"op": {"remove"}, "name": {"p"}})
	if got := u.cs.get().routeFor("claude-opus-5", "high"); got.Mode != "disabled" {
		t.Fatalf("removed provider left direct route: %+v", got)
	}
}

func TestDirectRouteParsesModelIDWithColon(t *testing.T) {
	u, h := testUI(t)
	u.cs.provPath = filepath.Join(t.TempDir(), "providers.json")
	l := u.cs.get().local.clone()
	l.Models = append(l.Models, localModel{Provider: "p", Model: "model:version"})
	if err := u.cs.applyLocal(l, false); err != nil {
		t.Fatal(err)
	}
	get(t, h, "POST", "/settings/route", url.Values{"model": {"opus"}, "scope": {"family"}, "high": {"model:p/model:version:high"}})
	if got := u.cs.get().routeFor("claude-opus-5", "high"); got.Mode != "model" || got.Model != "p/model:version" || got.Effort != "high" {
		t.Fatalf("colon in model ID parsed incorrectly: %+v", got)
	}
}

func TestDirectRouteRejectsUnlistedTargetEffort(t *testing.T) {
	u, h := testUI(t)
	u.cs.provPath = filepath.Join(t.TempDir(), "providers.json")
	l := u.cs.get().local.clone()
	if err := u.cs.applyLocal(l, false); err != nil {
		t.Fatal(err)
	}
	u.probe = map[string]probeResult{"p": {At: time.Now(), Base: l.Providers[0].BaseURL, Info: []probeModel{{ID: "m1", Efforts: []string{"low"}}}}}
	w := get(t, h, "POST", "/settings/route", url.Values{"model": {"opus"}, "scope": {"family"}, "high": {"model:p/m1:xhigh"}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `class="note err"`) {
		t.Fatalf("unsupported effort accepted: %d %s", w.Code, w.Body.String())
	}
}

func TestSettingsSaveDirectRouteAndReload(t *testing.T) {
	u, h := testUI(t)
	path := filepath.Join(t.TempDir(), "providers.json")
	u.cs.provPath = path
	body := get(t, h, "GET", "/settings", nil).Body.String()
	if !strings.Contains(body, `value="model:p/m1:high"`) ||
		!strings.Contains(body, `>p/m1/high</option>`) ||
		!strings.Contains(body, `value="model:p/m1:"`) ||
		!strings.Contains(body, `>p/m1</option>`) {
		t.Fatal("settings do not offer compact model/effort assignments")
	}
	w := get(t, h, "POST", "/settings/route", url.Values{
		"scope": {"family"}, "model": {"opus"}, "high": {"model:p/m1:high"},
	})
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `class="note err"`) {
		t.Fatalf("save direct route: %d %s", w.Code, w.Body.String())
	}
	loaded, err := readProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.routeFor("claude-opus-5-5", "high")
	if got.Mode != "model" || got.Model != "p/m1" || got.Effort != "high" {
		t.Fatalf("inherited direct route not persisted: %+v", got)
	}
}
