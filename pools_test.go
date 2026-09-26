package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"localrouter/internal/history"
)

func TestClaudePoolsSelectModelAndEffort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model  string `json:"model"`
			Effort string `json:"reasoning_effort"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q},"finish_reason":"stop"}]}`, body.Model+":"+body.Effort)
	}))
	defer server.Close()
	l := localSetup{
		Providers: []provider{{Name: "p", BaseURL: server.URL}},
		Models: []localModel{
			{Provider: "p", Model: "a", Efforts: map[string]string{"high": "xhigh"}},
			{Provider: "p", Model: "b", Efforts: map[string]string{"high": "medium"}},
		},
		ModelPools: map[string][]poolTarget{"deep": {{Model: "p/a", Effort: "xhigh"}, {Model: "p/b", Effort: "medium"}}, "fast": {{Model: "p/b", Effort: "low"}}},
		Routes: map[string]map[string]modelRoute{
			"claude-opus-5":    {"high": {Mode: "pool", Pool: "deep"}, "low": {Mode: "pool", Pool: "fast"}},
			"claude-sonnet-5":  {"high": {Mode: "pool", Pool: "fast"}},
			"claude-haiku-4-5": {"default": {Mode: "anthropic"}},
		},
	}
	if err := l.validate(); err != nil {
		t.Fatal(err)
	}
	cfg := config{local: l, failover: false}
	for _, tc := range []struct{ Model, Effort, Want string }{
		{"claude-opus-5", "high", "a:xhigh"},
		{"claude-opus-5", "low", "b:low"},
		{"claude-sonnet-5", "high", "b:low"},
	} {
		w := httptest.NewRecorder()
		body := fmt.Sprintf(`{"model":%q,"output_config":{"effort":%q},"messages":[{"role":"user","content":"hi"}]}`, tc.Model, tc.Effort)
		handleLocal(w, httptest.NewRequest("POST", "/v1/messages", nil), cfg.forModel(tc.Model, tc.Effort), []byte(body), nil, newHealth(""))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"text":"`+tc.Want+`"`) {
			t.Fatalf("%s: %d %s", tc.Model, w.Code, w.Body.String())
		}
	}
	bad := l.clone()
	bad.ModelPools["deep"] = []poolTarget{{Model: "missing/model"}}
	if err := bad.validate(); err == nil {
		t.Fatal("dangling pool target accepted")
	}
}

func TestCodexEffortMapping(t *testing.T) {
	if !validProviderEffort("ultra") || validProviderEffort("bogus") {
		t.Fatal("effort validation")
	}
	req := openaiRequest{Model: "gpt-test", Messages: []openaiMsg{{Role: "user", Content: "hello"}}, ReasoningEffort: "xhigh"}
	creq, err := toCodex(req)
	if err != nil || creq.Reasoning == nil || creq.Reasoning.Effort != "xhigh" {
		t.Fatalf("Codex effort: %+v %v", creq.Reasoning, err)
	}
}

func TestDashboardCodexProviderAndPoolRemoval(t *testing.T) {
	oldAuth := codexAuth
	codexAuth = &codexAuthStore{path: filepath.Join(t.TempDir(), "auth.json")}
	defer func() { codexAuth = oldAuth }()
	upstream, _ := url.Parse("https://api.anthropic.com")
	cs := newConfigStore(config{upstream: upstream}, filepath.Join(t.TempDir(), "providers.json"))
	u := newUIServer(history.New(10, ""), cs, newHealth(""))
	form := url.Values{"op": {"add"}, "name": {"codex"}, "type": {"codex"}, "base_url": {"http://127.0.0.1:1234/v1"}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/settings/providers", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	u.settingsProviders(w, r)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "class=\"note err\"") {
		t.Fatalf("add Codex provider: %d %s", w.Code, w.Body.String())
	}
	l := cs.get().local.clone()
	if len(l.Providers) != 1 || l.Providers[0].BaseURL != codexBaseURL {
		t.Fatalf("Codex endpoint not set: %+v", l.Providers)
	}
	l.Models = []localModel{{Provider: "codex", Model: "gpt-test"}}
	l.ModelPools = map[string][]poolTarget{"deep": {{Model: "codex/gpt-test"}}}
	if err := cs.applyLocal(l, true); err != nil {
		t.Fatal(err)
	}
	form = url.Values{"op": {"remove"}, "key": {"codex/gpt-test"}}
	r = httptest.NewRequest("POST", "/settings/models", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	u.settingsModels(w, r)
	if w.Code != http.StatusOK || len(cs.get().local.Models) != 0 || len(cs.get().local.ModelPools["deep"]) != 0 {
		t.Fatalf("remove model and pool reference: %d %+v", w.Code, cs.get().local)
	}
}
