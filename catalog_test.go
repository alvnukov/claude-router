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

	conf "localrouter/internal/config"
)

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

func TestCatalogRefreshMergesConcurrentEditsAndKeepsLastGood(t *testing.T) {
	u, _ := testUI(t)
	path := filepath.Join(t.TempDir(), "providers.json")
	u.cs = conf.NewStore(u.cs.Get(), path)
	entered, release := make(chan struct{}), make(chan struct{})
	u.fetchAnthropic = func(context.Context) ([]string, error) {
		close(entered)
		<-release
		return []string{"claude-opus-5-5", "claude-fable-5-1"}, nil
	}
	done := make(chan error, 1)
	go func() { done <- u.refreshModels(context.Background()) }()
	<-entered
	l := u.cs.Get().Local.Clone()
	l.FamilyRoutes = map[string]map[string]modelRoute{"opus": {"high": {Mode: "anthropic"}}}
	if err := u.cs.Update(conf.Replace(l, "")); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current := u.cs.Get().Local
	if current.RouteFor("claude-opus-5-5", "high").Mode != "anthropic" || len(current.Catalog.Anthropic) != 2 || len(current.Catalog.Providers["p"].Models) != 2 {
		t.Fatal("refresh overwrote routes or lost a catalog")
	}
	updated := current.Catalog.AnthropicUpdated
	u.fetchAnthropic = func(context.Context) ([]string, error) { return nil, errors.New("temporary outage") }
	if err := u.refreshModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := u.cs.Get().Local
	if !reflect.DeepEqual(after.Catalog.Anthropic, current.Catalog.Anthropic) || !after.Catalog.AnthropicUpdated.Equal(updated) || len(after.Catalog.Notes) != 1 {
		t.Fatal("failed refresh destroyed cache or hid failure")
	}
	reloaded, err := conf.ReadProviders(path)
	if err != nil || !reflect.DeepEqual(reloaded.Catalog.Anthropic, after.Catalog.Anthropic) || reloaded.RouteFor("claude-opus-6", "high").Mode != "anthropic" {
		t.Fatalf("catalog persistence: %v", err)
	}
}

func TestCodexEffortControlsUseSelectedModelCatalog(t *testing.T) {
	u, h := testUI(t)
	p := provider{Name: "codex", Type: "codex", BaseURL: conf.CodexBaseURL}
	l := localSetup{Providers: []provider{p}, Models: []localModel{{Provider: "codex", Model: "gpt-6-sol"}, {Provider: "codex", Model: "gpt-6-luna"}}, ModelPools: map[string][]poolTarget{"work": {}}}
	c := u.cs.Get()
	c.Local = l
	u.cs = conf.NewStore(c, filepath.Join(t.TempDir(), "providers.json"))
	u.probe = map[string]probeResult{"codex": {At: time.Now(), OK: true, Base: conf.CodexBaseURL, Models: []string{"gpt-6-sol", "gpt-6-luna"}, Info: []probeModel{{ID: "gpt-6-sol", Efforts: []string{"low", "high", "ultra"}}, {ID: "gpt-6-luna", Efforts: []string{"low", "high"}}}}}
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
	if len(u.cs.Get().Local.ModelPools["work"]) != 0 {
		t.Fatal("unsupported effort accepted")
	}
	get(t, h, "POST", "/settings/pools", url.Values{"name": {"work"}, "op": {"add"}, "key": {"codex/gpt-6-luna"}, "effort": {"high"}})
	if len(u.cs.Get().Local.ModelPools["work"]) != 1 {
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
	u.cs = conf.NewStore(u.cs.Get(), filepath.Join(t.TempDir(), "providers.json"))
	u.fetchAnthropic = func(context.Context) ([]string, error) { return []string{"claude-opus-5-5"}, nil }
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/settings/refresh-models", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Проверка моделей завершена") || len(u.cs.Get().Local.Catalog.Anthropic) != 1 {
		t.Fatalf("refresh: %d", w.Code)
	}
}
