package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	conf "localrouter/internal/config"
)

func TestLegacyMigrationPreservesExplicitRoutesAndEfforts(t *testing.T) {
	l := oneProvider("http://h/v1", "b", "a")
	l.Models[0].Efforts = map[string]string{"high": "xhigh", "max": "xhigh"}
	l.Pools = map[string][]string{"opus": {"p/a", "p/b"}, "haiku": {}}
	next, changed := conf.MigrateLegacyPools(l, []string{"claude-sonnet-5"})
	if !changed {
		t.Fatal("not migrated")
	}
	if err := next.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg := config{Local: next}
	if cfg.RouteFor("claude-sonnet-5", "high").Mode != "anthropic" || cfg.RouteFor("claude-haiku-4-5", "high").Mode != "anthropic" {
		t.Fatal("explicit Anthropic route lost")
	}
	scoped := cfg.ForModel("claude-opus-5", "high")
	if len(scoped.Local.Models) != 2 || scoped.Local.Models[0].Key() != "p/a" || scoped.Local.Models[1].Efforts["high"] != "xhigh" {
		t.Fatalf("pool/effort lost: %+v", scoped.Local.Models)
	}
	if cfg.RouteFor("unknown", "high").Mode != "disabled" {
		t.Fatal("unknown model implicitly enabled")
	}
	if cfg.RouteFor("claude-opus-5-5", "high").Mode != "disabled" {
		t.Fatal("new catalog model implicitly enabled by migration")
	}
	if _, changed = conf.MigrateLegacyPools(next, []string{"unknown"}); changed {
		t.Fatal("migration repeats")
	}
	if next.Pools != nil || next.Preferred != "" {
		t.Fatal("legacy configuration left active")
	}
	if l.Routes != nil || l.Models[0].Efforts["high"] != "xhigh" {
		t.Fatal("migration mutated original")
	}
	path := filepath.Join(t.TempDir(), "providers.json")
	original := []byte(`{"old":"config"}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := conf.SavePoolMigration(path, next); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".before-pools")
	if err != nil || string(backup) != string(original) {
		t.Fatal("backup missing")
	}
	reloaded, err := conf.ReadProviders(path)
	if err != nil || !reflect.DeepEqual(reloaded.Routes, next.Routes) || !reflect.DeepEqual(reloaded.ModelPools, next.ModelPools) {
		t.Fatalf("roundtrip: %v", err)
	}
	if err := conf.SavePoolMigration(path, next); err != nil {
		t.Fatal(err)
	}
	backup, _ = os.ReadFile(path + ".before-pools")
	if string(backup) != string(original) {
		t.Fatal("backup overwritten")
	}
}

func TestLegacyMigrationPreservesConfiguredLocalIDs(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		cloudOnly   []string
		pools       map[string][]string
	}{
		{name: "ordinary ID without cloud-only", model: "qwen3"},
		{name: "Claude ID even if cloud-only", model: "claude-opus-5", cloudOnly: []string{"claude-opus-5"}},
		{name: "configured ID matching empty legacy pool", model: "qwen3", pools: map[string][]string{"qwen3": {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := oneProvider("http://h/v1", tc.model)
			l.Pools = tc.pools
			next, changed := conf.MigrateLegacyPools(l, tc.cloudOnly)
			if !changed {
				t.Fatal("legacy configuration was not migrated")
			}
			cfg := config{Local: next}
			for _, effort := range conf.ClaudeEfforts {
				body := []byte(`{"model":"` + tc.model + `","output_config":{"effort":"` + effort + `"}}`)
				_, route, err := configuredRequestRoute(cfg, body)
				if err != nil || route.Mode != "pool" || len(next.ModelPools[route.Pool]) != 1 || next.ModelPools[route.Pool][0].Model != "p/"+tc.model {
					t.Fatalf("%s/%s lost local route: %+v, %v", tc.model, effort, route, err)
				}
			}
		})
	}
}

func TestLegacyMigrationKeepsFailoverForConfiguredModelID(t *testing.T) {
	l := oneProvider("http://h/v1", "qwen3", "fallback")
	next, changed := conf.MigrateLegacyPools(l, nil)
	if !changed {
		t.Fatal("legacy configuration was not migrated")
	}
	cfg := config{Local: next}
	_, route, err := configuredRequestRoute(cfg, []byte(`{"model":"qwen3"}`))
	if err != nil || route.Mode != "pool" {
		t.Fatalf("configured model lost its route: %+v, %v", route, err)
	}
	scoped := cfg.ForModel("qwen3", "default")
	if len(scoped.Local.Models) != 2 || scoped.Local.Models[0].Key() != "p/qwen3" || scoped.Local.Models[1].Key() != "p/fallback" {
		t.Fatalf("legacy failover targets lost: %+v", scoped.Local.Models)
	}
}

func TestFreshSetupHasNoImplicitRoutes(t *testing.T) {
	l := conf.SeedFromEnv()
	if _, changed := conf.MigrateLegacyPools(l, nil); changed || len(l.Routes) != 0 {
		t.Fatal("fresh setup silently enables Anthropic")
	}
}
