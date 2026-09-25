package main

import (
	"bytes"
	"compress/gzip"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSSE(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":10,"cache_read_input_tokens":4}}}`,
		``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
		``,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"Read","input":{}}}`,
		``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		``,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"x\"}"}}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`,
		``,
	}, "\n")
	p := parseResponse("text/event-stream", "", []byte(sse), false)
	if p.Model != "m" || p.StopReason != "tool_use" || p.Usage["input_tokens"] != 10 || p.Usage["output_tokens"] != 7 || p.Usage["cache_read_input_tokens"] != 4 {
		t.Fatalf("meta: %+v", p)
	}
	if len(p.Blocks) != 2 || p.Blocks[0].Text != "Hello" || p.Blocks[1].Name != "Read" || !strings.Contains(p.Blocks[1].Input, `"path": "x"`) {
		t.Fatalf("blocks: %+v", p.Blocks)
	}
}

func TestParseJSONError(t *testing.T) {
	p := parseResponse("application/json", "", []byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`), false)
	if p.Error != "overloaded_error: busy" {
		t.Fatalf("got %q", p.Error)
	}
}

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
	err := writeEnv(p, map[string]string{"ROUTER_LOCAL_MODEL": "new", "ROUTER_CLOUD_ONLY": "a,b", "ROUTER_LOCAL_API_KEY": "k y"})
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

func TestParseGzipResponse(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(`{"type":"message","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"hi"}],"usage":{"output_tokens":1}}`))
	zw.Close()
	p := parseResponse("application/json", "gzip", buf.Bytes(), false)
	if p.Note != "" || p.Error != "" || len(p.Blocks) != 1 || p.Blocks[0].Text != "hi" {
		t.Fatalf("%+v", p)
	}
	p = parseResponse("application/json", "br", []byte{3, 4, 5}, false)
	if p.Note == "" || p.Error != "" {
		t.Fatalf("br should be a note, got %+v", p)
	}
}

func TestUnassignedBackendIsDisabled(t *testing.T) {
	c := config{local: oneProvider("http://h/v1", "new")}
	for _, model := range []string{"old", "new", "p/new", "local-model"} {
		if c.routeFor(model, "default").Mode != "disabled" {
			t.Fatalf("unassigned %s is enabled", model)
		}
	}
}

func TestReadEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env")
	writeRaw(t, p, "# c\nexport A=1\nB=\"x y\"\nC='q'\nD=\nbad line\n")
	m, err := readEnv(p)
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
	cs := &configStore{c: config{failover: true, firstByte: 45 * time.Second}, envPath: p}
	changed, err := cs.reloadEnv()
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	c := cs.get()
	if c.failover || c.routeFor("claude-opus-5", "default").Mode != "disabled" || c.firstByte != 45*time.Second {
		t.Fatalf("%+v", c)
	}
	if changed, _ = cs.reloadEnv(); changed {
		t.Fatal("second reload should be a no-op")
	}
}

func TestProvidersFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "providers.json")
	cs := &configStore{c: config{local: oneProvider("http://h/v1", "zero")}, provPath: p}
	l := localSetup{
		Providers:  []provider{{Name: "a", BaseURL: "http://a/v1/"}, {Name: "b", BaseURL: "http://b/v1", APIKey: "k"}},
		Models:     []localModel{{Provider: "a", Model: "m1"}, {Provider: "b", Model: "m2"}, {Provider: "a", Model: "m1"}},
		ModelPools: map[string][]poolTarget{"work": {{Model: "b/m2", Effort: "high"}}},
		Routes:     map[string]map[string]modelRoute{"claude-opus-5": {"high": {Mode: "pool", Pool: "work"}}},
	}
	if err := cs.applyLocal(l, true); err != nil {
		t.Fatal(err)
	}
	requirePrivateFile(t, p)
	c := cs.get()
	if len(c.local.Models) != 2 || c.local.Providers[0].BaseURL != "http://a/v1" || c.routeFor("zero", "default").Mode != "disabled" || c.routeFor("m2", "default").Mode != "disabled" {
		t.Fatalf("%+v", c.local)
	}
	got, err := readProviders(p)
	if err != nil || got.Preferred != "" || got.routeFor("claude-opus-5", "high").Pool != "work" || got.ModelPools["work"][0].Effort != "high" || got.Providers[1].APIKey != "k" {
		t.Fatalf("%+v %v", got, err)
	}
	bad := localSetup{Providers: []provider{{Name: "x", BaseURL: "http://x/v1"}}, Models: []localModel{{Provider: "nope", Model: "m"}}}
	if err := cs.applyLocal(bad, false); err == nil {
		t.Fatal("model on unknown provider accepted")
	}
	bad = localSetup{Providers: []provider{{Name: "a/b", BaseURL: "http://x/v1"}}}
	if err := cs.applyLocal(bad, false); err == nil {
		t.Fatal("slash in provider name accepted")
	}
}

func TestHistoryPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.jsonl")
	st := newStore(2, p)
	for i := 0; i < 3; i++ {
		r := &record{Start: time.Now(), Model: "m", Route: "local", ReqBody: []byte(`{}`)}
		st.add(r)
		rw := newRecorder(httptest.NewRecorder(), 1<<20)
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(200)
		_, _ = rw.Write([]byte(`{"type":"message","content":[{"type":"text","text":"hi"}]}`))
		st.finish(r.ID, rw, nil)
	}
	st2 := newStore(2, p)
	got := st2.list()
	if len(got) != 2 || got[0].Resp == nil || got[0].Resp.Text() != "hi" || got[0].Seq != 2 {
		t.Fatalf("loaded %d records, first=%+v", len(got), got[0])
	}
	st2.clear()
	if b, _ := os.ReadFile(p); len(b) != 0 {
		t.Fatal("clear did not truncate the file")
	}
}

func TestSessionOf(t *testing.T) {
	body := []byte(`{"model":"x","metadata":{"user_id":"{\"device_id\":\"d\",\"account_uuid\":\"a\",\"session_id\":\"s-123\"}"},"messages":[]}`)
	if got := sessionOf(body); got != "s-123" {
		t.Fatalf("session = %q", got)
	}
	for _, b := range []string{`{"model":"x","messages":[]}`, `{"metadata":{"user_id":"plain"}}`, `not json`} {
		if got := sessionOf([]byte(b)); got != "" {
			t.Fatalf("%s: session = %q, want empty", b, got)
		}
	}
}
