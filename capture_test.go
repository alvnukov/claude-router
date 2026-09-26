package main

import (
	"strings"
	"testing"
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

func TestUnassignedBackendIsDisabled(t *testing.T) {
	c := config{Local: oneProvider("http://h/v1", "new")}
	for _, model := range []string{"old", "new", "p/new", "local-model"} {
		if c.RouteFor(model, "default").Mode != "disabled" {
			t.Fatalf("unassigned %s is enabled", model)
		}
	}
}
