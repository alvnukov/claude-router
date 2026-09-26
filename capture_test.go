package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	conf "localrouter/internal/config"
)

func TestBuildContext(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":10,"stream":true,
	 "system":[{"type":"text","text":"You are","cache_control":{"type":"ephemeral"}}],
	 "tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object"}}],
	 "messages":[
	  {"role":"user","content":"find the needle"},
	  {"role":"assistant","content":[{"type":"text","text":"ok"},{"type":"tool_use","id":"t1","name":"Read","input":{"path":"needle.txt"}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"needle needle"}],"is_error":true}]}
	 ]}`)
	v := buildContext(body, searchRegexp("NEEDLE"))
	if v.ParseErr != "" || len(v.System) != 1 || v.System[0].Cache != "ephemeral" || v.CacheMarks != 1 {
		t.Fatalf("system: %+v err=%s", v.System, v.ParseErr)
	}
	if len(v.Tools) != 1 || len(v.Messages) != 3 {
		t.Fatalf("tools=%d msgs=%d", len(v.Tools), len(v.Messages))
	}
	if v.Matches != 4 || v.Messages[0].Matches != 1 || v.Messages[1].Matches != 1 || v.Messages[2].Matches != 2 {
		t.Fatalf("matches total=%d per-msg=%d/%d/%d", v.Matches, v.Messages[0].Matches, v.Messages[1].Matches, v.Messages[2].Matches)
	}
	tr := v.Messages[2].Blocks[0]
	if tr.Kind != "tool_result" || tr.ToolUseID != "t1" || !tr.IsError || len(tr.Children) != 1 {
		t.Fatalf("tool_result: %+v", tr)
	}
	if !strings.Contains(string(tr.Children[0].HTML), "<mark>needle</mark>") {
		t.Fatalf("highlight: %s", tr.Children[0].HTML)
	}
	kinds := map[string]bool{}
	for _, k := range v.Kinds {
		kinds[k.Kind] = true
	}
	for _, want := range []string{"system", "tools", "user", "assistant", "tool_use", "tool_result"} {
		if !kinds[want] {
			t.Fatalf("missing kind %s in %+v", want, v.Kinds)
		}
	}
}

func TestWriteEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env")
	writeRaw(t, p, "# comment\nROUTER_LOCAL_MODEL=old\nOTHER=keep\n")
	err := conf.WriteEnv(p, map[string]string{"ROUTER_LOCAL_MODEL": "new", "ROUTER_CLOUD_ONLY": "a,b", "ROUTER_LOCAL_API_KEY": "k y"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := "# comment\nROUTER_LOCAL_MODEL=new\nOTHER=keep\nROUTER_CLOUD_ONLY=a,b\nROUTER_LOCAL_API_KEY=\"k y\"\n"
	if string(got) != want {
		t.Fatalf("got:\n%s", got)
	}
	requirePrivateFile(t, p)
}

func TestUnassignedBackendIsDisabled(t *testing.T) {
	c := config{Local: oneProvider("http://h/v1", "new")}
	for _, model := range []string{"old", "new", "p/new", "local-model"} {
		if c.RouteFor(model, "default").Mode != "disabled" {
			t.Fatalf("unassigned %s is enabled", model)
		}
	}
}

func TestReadEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env")
	writeRaw(t, p, "# c\nexport A=1\nB=\"x y\"\nC='q'\nD=\nbad line\n")
	m, err := conf.ReadEnv(p)
	if err != nil {
		t.Fatal(err)
	}
	if m["A"] != "1" || m["B"] != "x y" || m["C"] != "q" || m["D"] != "" {
		t.Fatalf("%+v", m)
	}
	if _, ok := m["bad line"]; ok {
		t.Fatal("bad line parsed")
	}
}

func TestReloadEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env")
	writeRaw(t, p, "ROUTER_CLOUD_ONLY=claude-opus-5\nROUTER_LOCAL_FAILOVER=0\n")
	t.Setenv("ROUTER_ENV_FILE", p)
	cs := conf.NewStore(config{Failover: true, FirstByte: 45 * time.Second}, "")
	changed, err := cs.ReloadEnv()
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	c := cs.Get()
	if c.Failover || c.RouteFor("claude-opus-5", "default").Mode != "disabled" || c.FirstByte != 45*time.Second {
		t.Fatalf("%+v", c)
	}
	if changed, _ = cs.ReloadEnv(); changed {
		t.Fatal("second reload should be a no-op")
	}
}

func TestProvidersFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "providers.json")
	cs := conf.NewStore(config{Local: oneProvider("http://h/v1", "zero")}, p)
	l := localSetup{
		Providers:  []provider{{Name: "a", BaseURL: "http://a/v1/"}, {Name: "b", BaseURL: "http://b/v1", APIKey: "k"}},
		Models:     []localModel{{Provider: "a", Model: "m1"}, {Provider: "b", Model: "m2"}, {Provider: "a", Model: "m1"}},
		ModelPools: map[string][]poolTarget{"work": {{Model: "b/m2", Effort: "high"}}},
		Routes:     map[string]map[string]modelRoute{"claude-opus-5": {"high": {Mode: "pool", Pool: "work"}}},
	}
	if err := cs.Update(conf.Replace(l, "")); err != nil {
		t.Fatal(err)
	}
	requirePrivateFile(t, p)
	c := cs.Get()
	if len(c.Local.Models) != 2 || c.Local.Providers[0].BaseURL != "http://a/v1" || c.RouteFor("zero", "default").Mode != "disabled" || c.RouteFor("m2", "default").Mode != "disabled" {
		t.Fatalf("%+v", c.Local)
	}
	got, err := conf.ReadProviders(p)
	if err != nil || got.Preferred != "" || got.RouteFor("claude-opus-5", "high").Pool != "work" || got.ModelPools["work"][0].Effort != "high" || got.Providers[1].APIKey != "k" {
		t.Fatalf("%+v %v", got, err)
	}
	bad := localSetup{Providers: []provider{{Name: "x", BaseURL: "http://x/v1"}}, Models: []localModel{{Provider: "nope", Model: "m"}}}
	if err := cs.Update(conf.Replace(bad, "")); err == nil {
		t.Fatal("model on unknown provider accepted")
	}
	bad = localSetup{Providers: []provider{{Name: "a/b", BaseURL: "http://x/v1"}}}
	if err := cs.Update(conf.Replace(bad, "")); err == nil {
		t.Fatal("slash in provider name accepted")
	}
}
