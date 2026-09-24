package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFamiliesInheritPerEffortWithVersionOverrides(t *testing.T) {
	l := oneProvider("http://h/v1", "a", "b")
	l.ModelPools = map[string][]poolTarget{"deep": {{Model: "p/a", Effort: "high"}}, "fast": {{Model: "p/b", Effort: "low"}}}
	l.Routes = map[string]map[string]modelRoute{
		"claude-opus-5":   {"high": {Mode: "pool", Pool: "deep"}, "low": {Mode: "anthropic"}},
		"claude-opus-4":   {"high": {Mode: "pool", Pool: "fast"}},
		"claude-sonnet-5": {"high": {Mode: "pool", Pool: "fast"}},
	}
	migrated, changed := migrateFamilyRoutes(l)
	if !changed {
		t.Fatal("not migrated")
	}
	if err := migrated.validate(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"claude-opus-5-5", "claude-opus-6", "opus"} {
		if migrated.routeFor(id, "high").Pool != "deep" || migrated.routeFor(id, "low").Mode != "anthropic" || migrated.routeFor(id, "").Mode != "disabled" {
			t.Fatalf("incorrect inherited routes for %s", id)
		}
	}
	if migrated.routeFor("claude-sonnet-6", "high").Pool != "fast" {
		t.Fatal("Sonnet inherited Opus")
	}
	if migrated.routeFor("claude-opus-5-other", "high").Mode != "disabled" || migrated.routeFor("claude-newfamily-1", "high").Mode != "disabled" {
		t.Fatal("unconfigured family enabled")
	}
	migrated.FamilyRoutes["opus"]["high"] = modelRoute{Mode: "anthropic"}
	if migrated.routeFor("claude-opus-5", "high").Mode != "anthropic" || migrated.routeFor("claude-opus-5-5", "high").Mode != "anthropic" {
		t.Fatal("inherited route did not follow family edit")
	}
	if migrated.routeFor("claude-opus-4", "high").Pool != "fast" {
		t.Fatal("version override lost")
	}
	migrated.Routes["claude-opus-5-5"] = map[string]modelRoute{"high": {Mode: "disabled"}}
	if migrated.routeFor("claude-opus-5-5", "high").Mode != "disabled" || migrated.routeFor("claude-opus-5-5", "low").Mode != "anthropic" {
		t.Fatal("per-effort overrides not respected")
	}
	if _, changed := migrateFamilyRoutes(migrated); changed {
		t.Fatal("family migration repeats")
	}
	if l.FamilyRoutes != nil || len(l.Routes) != 3 {
		t.Fatal("original mutated")
	}
}

func TestParseOfficialAnthropicCatalog(t *testing.T) {
	body := []byte(`<a href="/models/claude-opus-99">old documentation slug</a><button><span>claude-opus-5-5</span><svg></svg></button><button>claude-fable-5-1</button><button>claude-opus-5-5</button><button>anthropic.claude-opus-5-5</button><button>claude-haiku-4-5@20251001</button><script>claude-sonnet-99</script>`)
	got, err := parseAnthropicCatalog(body)
	if err != nil || !reflect.DeepEqual(got, []string{"claude-fable-5-1", "claude-opus-5-5"}) {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := parseAnthropicCatalog([]byte(`<html>upstream unavailable</html>`)); err == nil {
		t.Fatal("empty/error page accepted")
	}
}

func TestCodexNewVersionsInheritPoolAndSupportedEfforts(t *testing.T) {
	l := localSetup{Providers: []provider{{Name: "codex", Type: "codex", BaseURL: codexBaseURL}}, Models: []localModel{{Provider: "codex", Model: "gpt-6-sol"}}, ModelPools: map[string][]poolTarget{
		"high": {{Model: "codex/gpt-6-sol", Effort: "high"}}, "max": {{Model: "codex/gpt-6-sol", Effort: "max"}},
	}}
	models := []catalogModel{{ID: "gpt-6-sol", Efforts: []string{"high", "max"}}, {ID: "gpt-6.1-sol", Efforts: []string{"low", "high"}}, {ID: "gpt-6.1-luna", Efforts: []string{"high"}}, {ID: "gpt-5.6-sol", Efforts: []string{"high"}}}
	notes := inheritCodexModels(&l, "codex", models)
	if len(l.Models) != 4 {
		t.Fatalf("catalog not imported: %v", l.Models)
	}
	want := []poolTarget{{Model: "codex/gpt-6-sol", Effort: "high"}, {Model: "codex/gpt-6.1-sol", Effort: "high"}}
	if !reflect.DeepEqual(l.ModelPools["high"], want) {
		t.Fatalf("wrong pool inheritance: %+v", l.ModelPools["high"])
	}
	if len(l.ModelPools["max"]) != 1 || len(notes) != 1 || !strings.Contains(notes[0], "max") {
		t.Fatalf("unsupported effort inherited: %v %v", l.ModelPools, notes)
	}
	inheritCodexModels(&l, "codex", models)
	if len(l.ModelPools["high"]) != 2 || len(l.Models) != 4 {
		t.Fatal("duplicate import")
	}
	l.ModelPools["high"] = l.ModelPools["high"][:1]
	inheritCodexModels(&l, "codex", models)
	if len(l.ModelPools["high"]) != 1 {
		t.Fatal("manually removed member re-added")
	}
	if !newerModel("gpt-5.10-sol", "gpt-5.9-sol") || codexFamily("gpt-6-sol") == codexFamily("gpt-6-luna") {
		t.Fatal("version/family matching")
	}
}

func TestCatalogRefreshMergesConcurrentEditsAndKeepsLastGood(t *testing.T) {
	u, _ := testUI(t)
	u.cs.provPath = filepath.Join(t.TempDir(), "providers.json")
	entered, release := make(chan struct{}), make(chan struct{})
	u.fetchAnthropic = func(context.Context) ([]string, error) {
		close(entered)
		<-release
		return []string{"claude-opus-5-5", "claude-fable-5-1"}, nil
	}
	done := make(chan error, 1)
	go func() { done <- u.refreshModels(context.Background()) }()
	<-entered
	l := u.cs.get().local.clone()
	l.FamilyRoutes = map[string]map[string]modelRoute{"opus": {"high": {Mode: "anthropic"}}}
	if err := u.cs.applyLocal(l, true); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current := u.cs.get().local
	if current.routeFor("claude-opus-5-5", "high").Mode != "anthropic" || len(current.Catalog.Anthropic) != 2 || len(current.Catalog.Providers["p"].Models) != 2 {
		t.Fatal("refresh overwrote routes or lost a catalog")
	}
	updated := current.Catalog.AnthropicUpdated
	u.fetchAnthropic = func(context.Context) ([]string, error) { return nil, errors.New("temporary outage") }
	if err := u.refreshModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := u.cs.get().local
	if !reflect.DeepEqual(after.Catalog.Anthropic, current.Catalog.Anthropic) || !after.Catalog.AnthropicUpdated.Equal(updated) || len(after.Catalog.Notes) != 1 {
		t.Fatal("failed refresh destroyed cache or hid failure")
	}
	reloaded, err := readProviders(u.cs.provPath)
	if err != nil || !reflect.DeepEqual(reloaded.Catalog.Anthropic, after.Catalog.Anthropic) || reloaded.routeFor("claude-opus-6", "high").Mode != "anthropic" {
		t.Fatalf("catalog persistence: %v", err)
	}
}

func TestCodexEffortControlsUseSelectedModelCatalog(t *testing.T) {
	u, h := testUI(t)
	p := provider{Name: "codex", Type: "codex", BaseURL: codexBaseURL}
	l := localSetup{Providers: []provider{p}, Models: []localModel{{Provider: "codex", Model: "gpt-6-sol"}, {Provider: "codex", Model: "gpt-6-luna"}}, ModelPools: map[string][]poolTarget{"work": {}}}
	u.cs.c.local = l
	u.cs.provPath = filepath.Join(t.TempDir(), "providers.json")
	u.probe = map[string]probeResult{"codex": {At: time.Now(), OK: true, Base: codexBaseURL, Models: []string{"gpt-6-sol", "gpt-6-luna"}, Info: []probeModel{{ID: "gpt-6-sol", Efforts: []string{"low", "high", "ultra"}}, {ID: "gpt-6-luna", Efforts: []string{"low", "high"}}}}}
	for _, tc := range []struct {
		key   string
		ultra bool
	}{{"codex/gpt-6-sol", true}, {"codex/gpt-6-luna", false}} {
		body := get(t, h, "GET", "/settings/pool-add?"+url.Values{"name": {"work"}, "key": {tc.key}}.Encode(), nil).Body.String()
		if strings.Contains(body, `value="ultra"`) != tc.ultra || strings.Contains(body, `value="none"`) || strings.Contains(body, `value="minimal"`) {
			t.Fatalf("invented effort options: %s", body)
		}
	}
	get(t, h, "POST", "/settings/pools", url.Values{"name": {"work"}, "op": {"add"}, "key": {"codex/gpt-6-luna"}, "effort": {"ultra"}})
	if len(u.cs.get().local.ModelPools["work"]) != 0 {
		t.Fatal("unsupported effort accepted")
	}
	get(t, h, "POST", "/settings/pools", url.Values{"name": {"work"}, "op": {"add"}, "key": {"codex/gpt-6-luna"}, "effort": {"high"}})
	if len(u.cs.get().local.ModelPools["work"]) != 1 {
		t.Fatal("supported effort rejected")
	}
	if opts := modelEffortOptions(l, "codex/gpt-6-sol", nil); len(opts) != 0 {
		t.Fatal("missing catalog fell back to invented levels")
	}
	if opts := modelEffortOptions(l, "codex/gpt-6-sol", map[string]probeModel{"codex/gpt-6-sol": {}}); slices.Contains(opts, "high") {
		t.Fatal("empty advertised levels ignored")
	}
}

func TestGlobalRefreshButtonUsesSameUpdater(t *testing.T) {
	u, h := testUI(t)
	u.fetchAnthropic = func(context.Context) ([]string, error) { return []string{"claude-opus-5-5"}, nil }
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/settings/refresh-models", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Проверка моделей завершена") || len(u.cs.get().local.Catalog.Anthropic) != 1 {
		t.Fatalf("refresh: %d", w.Code)
	}
}

func TestCodexInheritanceIncludesInactiveProfilesOnce(t *testing.T) {
	l := localSetup{Providers: []provider{{Name: "codex", Type: "codex", BaseURL: codexBaseURL}}, Models: []localModel{{Provider: "codex", Model: "gpt-6-sol"}}, ModelPools: map[string][]poolTarget{"work": {{Model: "codex/gpt-6-sol", Effort: "high"}}}}
	l.Profiles = map[string]routingProfile{"default": l.routing(), "cloud": l.routing()}
	l.ActiveProfile = "default"
	models := []catalogModel{{ID: "gpt-6-sol", Efforts: []string{"high"}}, {ID: "gpt-6.1-sol", Efforts: []string{"high"}}}
	inheritCodexModels(&l, "codex", models)
	if len(l.ModelPools["work"]) != 2 || len(l.Profiles["cloud"].ModelPools["work"]) != 2 {
		t.Fatalf("new version missed profile: %+v", l.Profiles)
	}
	cloud := l.Profiles["cloud"]
	cloud.ModelPools["work"] = cloud.ModelPools["work"][:1]
	l.Profiles["cloud"] = cloud
	inheritCodexModels(&l, "codex", models)
	if len(l.Profiles["cloud"].ModelPools["work"]) != 1 {
		t.Fatal("manually removed version re-added")
	}
}
