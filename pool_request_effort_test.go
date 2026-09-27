package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestPoolPreservesRequestEffortThroughFailover(t *testing.T) {
	for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		t.Run("effort="+effort, func(t *testing.T) {
			var seen []string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body openaiRequest
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				seen = append(seen, body.Model+":"+body.ReasoningEffort)
				if body.Model == "a" {
					http.Error(w, `{"error":{"message":"try next"}}`, http.StatusServiceUnavailable)
					return
				}
				fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
			}))
			defer up.Close()
			source := effort
			if source == "" {
				source = "default"
			}
			cfg := config{failover: true, local: localSetup{
				Providers:    []provider{{Name: "p", BaseURL: up.URL}},
				Models:       []localModel{{Provider: "p", Model: "a"}, {Provider: "p", Model: "b"}},
				ModelPools:   map[string][]poolTarget{"same": {{Model: "p/a", Effort: poolRequestEffort}, {Model: "p/b", Effort: poolRequestEffort}}},
				FamilyRoutes: map[string]map[string]modelRoute{"sonnet": {source: {Mode: "pool", Pool: "same"}}},
			}}
			if err := cfg.local.validate(); err != nil {
				t.Fatal(err)
			}
			body := []byte(fmt.Sprintf(`{"model":"claude-sonnet-5","output_config":{"effort":%q},"messages":[{"role":"user","content":"hi"}]}`, effort))
			w := httptest.NewRecorder()
			handleLocal(w, httptest.NewRequest("POST", "/v1/messages", nil), cfg.forModel("claude-sonnet-5", effort), body, nil, newHealth(""))
			if w.Code != 200 || !reflect.DeepEqual(seen, []string{"a:" + effort, "b:" + effort}) {
				t.Fatalf("status=%d upstream=%v body=%s", w.Code, seen, w.Body.String())
			}
			if cfg.local.ModelPools["same"][0].Effort != poolRequestEffort {
				t.Fatal("request mutated the saved policy")
			}
		})
	}
}

func TestPoolRequestEffortMixedWithFixedAndCodex(t *testing.T) {
	l := localSetup{
		Models:       []localModel{{Provider: "p", Model: "a"}, {Provider: "p", Model: "b"}, {Provider: "p", Model: "c"}},
		ModelPools:   map[string][]poolTarget{"mixed": {{Model: "p/a", Effort: poolRequestEffort}, {Model: "p/b", Effort: "low"}, {Model: "p/c"}}},
		FamilyRoutes: map[string]map[string]modelRoute{"sonnet": {}},
	}
	for _, effort := range []string{"default", "high", "medium"} {
		l.FamilyRoutes["sonnet"][effort] = modelRoute{Mode: "pool", Pool: "mixed"}
		cfg := (config{local: l}).forModel("claude-sonnet-5", effort)
		want := effort
		if effort == "default" {
			want = ""
		}
		for i, expected := range []string{want, "low", ""} {
			actual := cfg.local.Models[i].Efforts[effort]
			if actual != expected {
				t.Fatalf("%s model %d: %q, want %q", effort, i, actual, expected)
			}
			converted, err := toCodex(openaiRequest{Model: "gpt", Messages: []openaiMsg{{Role: "user", Content: "hi"}}, ReasoningEffort: actual})
			if err != nil {
				t.Fatal(err)
			}
			if expected == "" {
				if converted.Reasoning != nil {
					t.Fatal("absent effort was added to Codex request")
				}
			} else if converted.Reasoning == nil || converted.Reasoning.Effort != expected {
				t.Fatalf("Codex effort lost: %+v", converted.Reasoning)
			}
		}
	}
}

func TestCodexCatalogInheritsRequestEffort(t *testing.T) {
	l := localSetup{
		Models:     []localModel{{Provider: "codex", Model: "gpt-6-sol"}},
		ModelPools: map[string][]poolTarget{"same": {{Model: "codex/gpt-6-sol", Effort: poolRequestEffort}}},
	}
	l.ActiveProfile = "active"
	l.Profiles = map[string]routingProfile{"active": l.routing(), "other": l.routing()}
	inheritCodexModels(&l, "codex", []catalogModel{{ID: "gpt-6.1-sol", Efforts: []string{"low", "high"}}})
	for _, pools := range []map[string][]poolTarget{l.ModelPools, l.Profiles["other"].ModelPools} {
		members := pools["same"]
		if len(members) != 2 || !reflect.DeepEqual(members[1], poolTarget{Model: "codex/gpt-6.1-sol", Effort: poolRequestEffort}) {
			t.Fatalf("catalog lost request effort mode: %+v", members)
		}
	}
}

func TestPoolEffortMapOverridesFallbackAndClonesIndependently(t *testing.T) {
	for _, fallback := range []string{"", "high", poolRequestEffort} {
		original := localSetup{ModelPools: map[string][]poolTarget{"work": {{Model: "p/a", Effort: fallback, EffortMap: map[string]string{"default": "low", "medium": "", "high": "xhigh", "max": poolRequestEffort}}}}}
		member := original.ModelPools["work"][0]
		for source, want := range map[string]string{"default": "low", "medium": "", "high": "xhigh", "max": "max"} {
			if got := member.requestEffort(source); got != want {
				t.Fatalf("%s/%s: %q, want %q", fallback, source, got, want)
			}
		}
		want := fallback
		if fallback == poolRequestEffort {
			want = "low"
		}
		if member.requestEffort("low") != want {
			t.Fatal("missing override did not use fallback")
		}
		copy := original.clone()
		copy.ModelPools["work"][0].EffortMap["high"] = "medium"
		if original.ModelPools["work"][0].EffortMap["high"] != "xhigh" {
			t.Fatal("snapshot aliases effort mapping")
		}
	}
	member := poolTarget{Effort: poolRequestEffort, EffortMap: map[string]string{"high": "xhigh"}}
	if got := member.unsupportedEffort([]string{"low", "high"}); got != "xhigh" {
		t.Fatalf("catalog must check fixed overrides: %q", got)
	}
}
