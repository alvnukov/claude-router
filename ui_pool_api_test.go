package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	webui "localrouter/internal/ui"
)

func TestUIJSONPoolCloneAndRequestEffort(t *testing.T) {
	for _, explicitSettings := range []bool{false, true} {
		t.Run(map[bool]string{false: "defaults", true: "explicit"}[explicitSettings], func(t *testing.T) {
			u, h := testUI(t)
			u.cs.provPath = filepath.Join(t.TempDir(), "providers.json")
			l := u.cs.get().local.clone()
			l.Models = append(l.Models, localModel{Provider: "p", Model: "m2"})
			l.ModelPools = map[string][]poolTarget{"source": {{Model: "p/m1", Effort: "high"}, {Model: "p/m2", Effort: "low"}}}
			if explicitSettings {
				l.PoolSettings = map[string]poolSettings{"source": {Type: poolBalance, Failover: false, FirstByteSec: 7, ProbeSec: 9, MaxInputChars: 12345}}
			}
			l.ActiveProfile = "active"
			l.Profiles = map[string]routingProfile{"active": l.routing(), "other": l.routing()}
			u.cs.c.local = l
			if err := u.cs.applyLocal(l, true); err != nil {
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
			action("pool.edit", map[string]string{"op": "effort", "name": "source", "key": "p/m1", "effort": poolRequestEffort})
			action("pool.edit", map[string]string{"op": "mapping", "name": "source", "key": "p/m1", "default": "inherit", "low": "inherit", "medium": "low", "high": "xhigh", "xhigh": "inherit", "max": ""})
			original := u.cs.get()
			action("pool.edit", map[string]string{"op": "clone", "source": "source", "name": "copy"})
			got := u.cs.get()
			if !reflect.DeepEqual(got.local.ModelPools["copy"], original.local.ModelPools["source"]) || got.poolSettings("copy") != original.poolSettings("source") {
				t.Fatalf("clone lost order/effort/settings: %+v", got.local)
			}
			if !reflect.DeepEqual(got.local.Routes, original.local.Routes) || !reflect.DeepEqual(got.local.Profiles["other"], original.local.Profiles["other"]) {
				t.Fatal("clone changed routes or another profile")
			}
			action("pool.edit", map[string]string{"op": "down", "name": "copy", "key": "p/m1"})
			got = u.cs.get()
			if got.local.ModelPools["copy"][0].Model != "p/m2" || !reflect.DeepEqual(got.local.ModelPools["source"], original.local.ModelPools["source"]) {
				t.Fatal("clone aliases original members")
			}
			action("route.save", map[string]string{"scope": "family", "model": "sonnet", "all": "pool:copy"})
			reloaded, err := readProviders(u.cs.provPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, effort := range claudeEfforts {
				cfg := (config{local: reloaded}).forModel("claude-sonnet-5", effort)
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
				if len(cfg.local.Models) != 2 || cfg.local.Models[1].Efforts[effort] != want {
					t.Fatalf("route/policy lost after reload: %s %+v", effort, cfg.local.Models)
				}
			}
			before, err := os.ReadFile(u.cs.provPath)
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
				after, err := os.ReadFile(u.cs.provPath)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("rejected action changed persisted config")
				}
			}
			action("pool.edit", map[string]string{"op": "mapping", "name": "copy", "key": "p/m1", "default": "inherit", "low": "inherit", "medium": "inherit", "high": "inherit", "xhigh": "inherit", "max": "inherit"})
			got = u.cs.get()
			if len(got.local.ModelPools["copy"][1].EffortMap) != 0 || got.local.ModelPools["source"][0].EffortMap["high"] != "xhigh" {
				t.Fatal("resetting a copy changed the source mapping")
			}
		})
	}
}
