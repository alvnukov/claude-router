package config

import "testing"

func TestFamiliesInheritPerEffortWithVersionOverrides(t *testing.T) {
	l := oneProvider("http://h/v1", "a", "b")
	l.ModelPools = map[string][]PoolTarget{"deep": {{Model: "p/a", Effort: "high"}}, "fast": {{Model: "p/b", Effort: "low"}}}
	l.Routes = map[string]map[string]Route{
		"claude-opus-5":   {"high": {Mode: "pool", Pool: "deep"}, "low": {Mode: "anthropic"}},
		"claude-opus-4":   {"high": {Mode: "pool", Pool: "fast"}},
		"claude-sonnet-5": {"high": {Mode: "pool", Pool: "fast"}},
	}
	migrated, changed := migrateFamilyRoutes(l)
	if !changed {
		t.Fatal("not migrated")
	}
	if err := migrated.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"claude-opus-5-5", "claude-opus-6", "opus"} {
		if migrated.RouteFor(id, "high").Pool != "deep" || migrated.RouteFor(id, "low").Mode != "anthropic" || migrated.RouteFor(id, "").Mode != "disabled" {
			t.Fatalf("incorrect inherited routes for %s", id)
		}
	}
	if migrated.RouteFor("claude-sonnet-6", "high").Pool != "fast" {
		t.Fatal("Sonnet inherited Opus")
	}
	if migrated.RouteFor("claude-opus-5-other", "high").Mode != "disabled" || migrated.RouteFor("claude-newfamily-1", "high").Mode != "disabled" {
		t.Fatal("unconfigured family enabled")
	}
	migrated.FamilyRoutes["opus"]["high"] = Route{Mode: "anthropic"}
	if migrated.RouteFor("claude-opus-5", "high").Mode != "anthropic" || migrated.RouteFor("claude-opus-5-5", "high").Mode != "anthropic" {
		t.Fatal("inherited route did not follow family edit")
	}
	if migrated.RouteFor("claude-opus-4", "high").Pool != "fast" {
		t.Fatal("version override lost")
	}
	migrated.Routes["claude-opus-5-5"] = map[string]Route{"high": {Mode: "disabled"}}
	if migrated.RouteFor("claude-opus-5-5", "high").Mode != "disabled" || migrated.RouteFor("claude-opus-5-5", "low").Mode != "anthropic" {
		t.Fatal("per-effort overrides not respected")
	}
	if _, changed := migrateFamilyRoutes(migrated); changed {
		t.Fatal("family migration repeats")
	}
	if l.FamilyRoutes != nil || len(l.Routes) != 3 {
		t.Fatal("original mutated")
	}
}
