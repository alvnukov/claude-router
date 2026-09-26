package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestCodexNewVersionsInheritPoolAndSupportedEfforts(t *testing.T) {
	l := Local{Providers: []Provider{{Name: "codex", Type: "codex", BaseURL: CodexBaseURL}}, Models: []Model{{Provider: "codex", Model: "gpt-6-sol"}}, ModelPools: map[string][]PoolTarget{
		"high": {{Model: "codex/gpt-6-sol", Effort: "high"}}, "max": {{Model: "codex/gpt-6-sol", Effort: "max"}},
	}}
	models := []CatalogModel{{ID: "gpt-6-sol", Efforts: []string{"high", "max"}}, {ID: "gpt-6.1-sol", Efforts: []string{"low", "high"}}, {ID: "gpt-6.1-luna", Efforts: []string{"high"}}, {ID: "gpt-5.6-sol", Efforts: []string{"high"}}}
	notes := inheritCodexModels(&l, "codex", models)
	if len(l.Models) != 4 {
		t.Fatalf("catalog not imported: %v", l.Models)
	}
	want := []PoolTarget{{Model: "codex/gpt-6-sol", Effort: "high"}, {Model: "codex/gpt-6.1-sol", Effort: "high"}}
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

func TestCodexInheritanceIncludesInactiveProfilesOnce(t *testing.T) {
	l := Local{Providers: []Provider{{Name: "codex", Type: "codex", BaseURL: CodexBaseURL}}, Models: []Model{{Provider: "codex", Model: "gpt-6-sol"}}, ModelPools: map[string][]PoolTarget{"work": {{Model: "codex/gpt-6-sol", Effort: "high"}}}}
	l.Profiles = map[string]Profile{"default": l.routing(), "cloud": l.routing()}
	l.ActiveProfile = "default"
	models := []CatalogModel{{ID: "gpt-6-sol", Efforts: []string{"high"}}, {ID: "gpt-6.1-sol", Efforts: []string{"high"}}}
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
