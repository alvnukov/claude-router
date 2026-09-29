package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	conf "localrouter/internal/config"
	webui "localrouter/internal/ui"
)

func TestUIJSONPoolCloneAndRequestEffort(t *testing.T) {
	for _, explicitSettings := range []bool{false, true} {
		t.Run(map[bool]string{false: "defaults", true: "explicit"}[explicitSettings], func(t *testing.T) {
			u, h := testUI(t)
			u.cs = conf.NewStore(u.cs.Get(), filepath.Join(t.TempDir(), "providers.json"))
			l := u.cs.Get().Local.Clone()
			l.Models = append(l.Models, localModel{Provider: "p", Model: "m2"})
			l.ModelPools = map[string][]poolTarget{"source": {{Model: "p/m1", Effort: "high"}, {Model: "p/m2", Effort: "low"}}}
			if explicitSettings {
				l.PoolSettings = map[string]poolSettings{"source": {Type: conf.PoolBalance, Failover: false, FirstByteSec: 7, ProbeSec: 9, MaxInputChars: 12345}}
			}
			l.ActiveProfile = "active"
			l.Profiles = map[string]conf.Profile{"active": l.Routing(), "other": l.Routing()}
			if err := u.cs.Update(func(cur *localSetup) error { *cur = l; return nil }); err != nil {
				t.Fatal(err)
			}
			action := func(name string, fields map[string]string) {
				t.Helper()
				fields["profile"] = "active"
				w := apiCall(t, h, "POST", "/api/ui/actions", webui.Action{Action: name, Fields: fields})
				if w.Code != 200 {
					t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
				}
			}
			action("pool.edit", map[string]string{"op": "effort", "name": "source", "key": "p/m1", "effort": conf.PoolRequestEffort})
			action("pool.edit", map[string]string{"op": "mapping", "name": "source", "key": "p/m1", "default": "inherit", "low": "inherit", "medium": "low", "high": "xhigh", "xhigh": "inherit", "max": ""})
			original := u.cs.Get()
			action("pool.edit", map[string]string{"op": "clone", "source": "source", "name": "copy"})
			got := u.cs.Get()
			if !reflect.DeepEqual(got.Local.ModelPools["copy"], original.Local.ModelPools["source"]) || got.PoolSettings("copy") != original.PoolSettings("source") {
				t.Fatalf("clone lost order/effort/settings: %+v", got.Local)
			}
			if !reflect.DeepEqual(got.Local.Routes, original.Local.Routes) || !reflect.DeepEqual(got.Local.Profiles["other"], original.Local.Profiles["other"]) {
				t.Fatal("clone changed routes or another profile")
			}
			action("pool.edit", map[string]string{"op": "down", "name": "copy", "key": "p/m1"})
			got = u.cs.Get()
			if got.Local.ModelPools["copy"][0].Model != "p/m2" || !reflect.DeepEqual(got.Local.ModelPools["source"], original.Local.ModelPools["source"]) {
				t.Fatal("clone aliases original members")
			}
			action("route.save", map[string]string{"scope": "family", "model": "sonnet", "all": "pool:copy"})
			reloaded, err := conf.ReadProviders(u.cs.Path())
			if err != nil {
				t.Fatal(err)
			}
			for _, effort := range conf.ClaudeEfforts {
				cfg := (config{Local: reloaded}).ForModel("claude-sonnet-5", effort)
				want := effort
				if effort == "default" {
					want = ""
				}
				if effort == "medium" {
					want = "low"
				}
				if effort == "high" {
					want = "xhigh"
				}
				if effort == "max" {
					want = ""
				}
				if len(cfg.Local.Models) != 2 || cfg.Local.Models[1].Efforts[effort] != want {
					t.Fatalf("route/policy lost after reload: %s %+v", effort, cfg.Local.Models)
				}
			}
			before, err := os.ReadFile(u.cs.Path())
			if err != nil {
				t.Fatal(err)
			}
			for _, fields := range []map[string]string{
				{"profile": "active", "op": "clone", "source": "source", "name": "copy"},
				{"profile": "active", "op": "clone", "source": "missing", "name": "new"},
				{"profile": "active", "op": "clone", "source": "source", "name": " "},
				{"profile": "other", "op": "clone", "source": "source", "name": "new"},
				{"profile": "active", "op": "effort", "name": "copy", "key": "p/m1", "effort": "invalid"},
				{"profile": "active", "op": "mapping", "name": "copy", "key": "p/m1", "high": "low"},
				{"profile": "active", "op": "mapping", "name": "copy", "key": "p/m1", "default": "inherit", "low": "inherit", "medium": "low", "high": "invalid", "xhigh": "inherit", "max": "inherit"},
			} {
				w := apiCall(t, h, "POST", "/api/ui/actions", webui.Action{Action: "pool.edit", Fields: fields})
				if w.Code != 400 {
					t.Fatalf("invalid action accepted: %+v: %d", fields, w.Code)
				}
				after, err := os.ReadFile(u.cs.Path())
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("rejected action changed persisted config")
				}
			}
			action("pool.edit", map[string]string{"op": "mapping", "name": "copy", "key": "p/m1", "default": "inherit", "low": "inherit", "medium": "inherit", "high": "inherit", "xhigh": "inherit", "max": "inherit"})
			got = u.cs.Get()
			if len(got.Local.ModelPools["copy"][1].EffortMap) != 0 || got.Local.ModelPools["source"][0].EffortMap["high"] != "xhigh" {
				t.Fatal("resetting a copy changed the source mapping")
			}
		})
	}
}
