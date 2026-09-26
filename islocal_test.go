package main

import (
	"fmt"
	"testing"
)

func TestExplicitModelEffortRoutes(t *testing.T) {
	l := oneProvider("http://h/v1", "a")
	l.ModelPools = map[string][]poolTarget{"work": {{Model: "p/a", Effort: "high"}}, "empty": {}}
	l.Routes = map[string]map[string]modelRoute{
		"claude-opus-5": {"high": {Mode: "pool", Pool: "work"}, "low": {Mode: "anthropic"}, "max": {Mode: "pool", Pool: "empty"}},
	}
	cfg := config{Local: l}
	for _, tc := range []struct {
		model, effort, mode string
		rejected            bool
	}{
		{"claude-opus-5", "high", "pool", false},
		{"claude-opus-5", "low", "anthropic", false},
		{"claude-opus-5", "max", "pool", true},
		{"claude-opus-5", "", "disabled", true},
		{"claude-opus-5", "ultra", "disabled", true},
		{"claude-sonnet-5", "high", "disabled", true},
		{"claude-opus-5-other", "high", "disabled", true},
		{"local-model", "high", "disabled", true},
		{"p/a", "high", "disabled", true},
	} {
		body := []byte(fmt.Sprintf(`{"model":%q,"output_config":{"effort":%q}}`, tc.model, tc.effort))
		_, route, err := configuredRequestRoute(cfg, body)
		if route.Mode != tc.mode || (err != nil) != tc.rejected {
			t.Errorf("%s/%s: route=%+v err=%v", tc.model, tc.effort, route, err)
		}
	}
}
