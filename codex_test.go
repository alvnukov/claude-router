package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The ChatGPT subscription endpoint rejects max_output_tokens with HTTP 400;
// every request that carried it failed in production.
func TestCodexRequestOmitsOutputLimit(t *testing.T) {
	out, err := toCodex(openaiRequest{Model: "gpt-test", MaxTokens: 32, Messages: []openaiMsg{{Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if _, ok := request["max_output_tokens"]; ok {
		t.Fatalf("Codex request carries max_output_tokens, which the endpoint rejects: %s", body)
	}
}

// Codex CLI's ResponsesApiRequest (codex-rs/codex-api/src/common.rs) has no
// sampling fields; the subscription endpoint is only known to accept its shape.
func TestCodexRequestOmitsSamplingFields(t *testing.T) {
	temp, topP := 0.5, 0.9
	out, err := toCodex(openaiRequest{Model: "gpt-test", Temperature: &temp, TopP: &topP, Messages: []openaiMsg{{Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"temperature", "top_p"} {
		if _, ok := request[field]; ok {
			t.Fatalf("Codex request carries %s: %s", field, body)
		}
	}
}

func TestCodexRequestAndResponse(t *testing.T) {
	in := openaiRequest{Model: "gpt-test", Messages: []openaiMsg{
		{Role: "system", Content: "system"},
		{Role: "user", Content: "write code"},
		{Role: "assistant", ToolCalls: []openaiToolCall{{ID: "call-1", Type: "function", Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "shell", Arguments: `{"cmd":"pwd"}`}}}},
		{Role: "tool", ToolCallID: "call-1", Content: "ok"},
	}, ToolChoice: map[string]any{"type": "function", "function": map[string]any{"name": "shell"}}}
	out, err := toCodex(in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Model != "gpt-test" || out.Instructions != "system" || len(out.Input) != 3 || out.Store || !out.Stream {
		t.Fatalf("bad Responses request: %+v", out)
	}
	choice, _ := out.ToolChoice.(map[string]any)
	if choice["name"] != "shell" {
		t.Fatalf("tool choice: %+v", choice)
	}
	stream := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		`data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call-2","name":"shell","arguments":"{\"cmd\":\"ls\"}"}}`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":12,"output_tokens":3}}}`,
	}, "\n\n")
	result, err := readCodexEvents(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if result.Text.String() != "hello" || len(result.Tools) != 1 || result.Tools[0].Function.Name != "shell" || result.InputTokens != 12 {
		t.Fatalf("bad result: %+v", result)
	}
	w := httptest.NewRecorder()
	if err := blockingResponse(w, strings.NewReader(string(codexAsChatResponse(result))), "gpt-test"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Body.String(), `"type":"tool_use"`) || !strings.Contains(w.Body.String(), `"text":"hello"`) {
		t.Fatalf("bad Anthropic response: %s", w.Body.String())
	}
	sw := httptest.NewRecorder()
	pipe := codexChatStream(strings.NewReader(stream))
	defer pipe.Close()
	if err := streamResponse(sw, pipe, "gpt-test"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sw.Body.String(), "event: message_stop") || !strings.Contains(sw.Body.String(), `"partial_json":"{\"cmd\":\"ls\"}"`) {
		t.Fatalf("bad Anthropic stream: %s", sw.Body.String())
	}
	toolStream := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call-3","name":"shell","arguments":""}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"cmd\":"}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"\"pwd\"}"}`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call-3","name":"shell","arguments":"{\"cmd\":\"pwd\"}"}}`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`,
	}, "\n\n")
	toolOut := httptest.NewRecorder()
	if err := streamResponse(toolOut, codexChatStream(strings.NewReader(toolStream)), "gpt-test"); err != nil {
		t.Fatal(err)
	}
	if strings.Count(toolOut.Body.String(), `"type":"input_json_delta"`) != 2 || !strings.Contains(toolOut.Body.String(), `"name":"shell"`) {
		t.Fatalf("tool arguments did not stream: %s", toolOut.Body.String())
	}
	if _, err := readCodexEvents(strings.NewReader(`data: {"type":"response.output_text.delta","delta":"partial"}`)); err == nil {
		t.Fatal("incomplete Codex response accepted")
	}
	broken := codexChatStream(strings.NewReader(`data: {"type":"response.output_text.delta","delta":"partial"}`))
	defer broken.Close()
	if _, err := io.ReadAll(broken); err == nil {
		t.Fatal("incomplete Codex stream accepted")
	}
}

func TestCodexRejectsUnpairedToolOutput(t *testing.T) {
	_, err := toCodex(openaiRequest{Model: "gpt-test", Messages: []openaiMsg{
		{Role: "tool", ToolCallID: "call-1", Content: "tool result"},
	}})
	if err == nil {
		t.Fatal("sent a function_call_output without its function_call")
	}
}

func TestCodexStreamDoesNotWaitForCompletion(t *testing.T) {
	upstream, write := io.Pipe()
	stream := codexChatStream(upstream)
	defer upstream.Close()
	defer write.Close()
	defer stream.Close()
	first := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stream).ReadString('\n')
		first <- line
	}()
	if _, err := io.WriteString(write, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"first\"}\n\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-first:
		if !strings.Contains(line, `"content":"first"`) {
			t.Fatalf("first streamed chunk: %s", line)
		}
	case <-time.After(time.Second):
		t.Fatal("Codex adapter buffered text until completion")
	}
}

func TestCodexCredentialRefreshAndPin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.FormValue("refresh_token") != "old-refresh" {
			t.Error("refresh token missing")
		}
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"new-refresh"}`, testJWT(time.Now().Add(time.Hour)))
	}))
	defer server.Close()
	dir := t.TempDir()
	s := &codexAuthStore{path: filepath.Join(dir, "router.json"), cliPath: filepath.Join(dir, "cli.json"), issuer: server.URL, client: server.Client()}
	var c codexCredential
	c.AuthMode = "chatgpt"
	c.Tokens.AccountID = "acct"
	c.Tokens.AccessToken = testJWT(time.Now().Add(-time.Minute))
	c.Tokens.RefreshToken = "old-refresh"
	data, _ := json.Marshal(c)
	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.credentialFor(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got.Tokens.RefreshToken != "new-refresh" {
		t.Fatal("refresh token not rotated")
	}
	st, err := os.Stat(s.path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("credential permissions: %v %v", st, err)
	}
	req := httptest.NewRequest("POST", codexBaseURL+"/responses", nil)
	if err := s.authorize(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("ChatGPT-Account-Id") != "acct" {
		t.Fatal("account header missing")
	}
	for _, target := range []string{"https://other.test/backend-api/codex/responses", "http://chatgpt.com/backend-api/codex/responses", "https://chatgpt.com/other"} {
		req := httptest.NewRequest("POST", target, nil)
		if err := s.authorize(t.Context(), req); err == nil {
			t.Fatalf("authorized %s", target)
		}
	}
}

func testJWT(exp time.Time) string {
	data, _ := json.Marshal(map[string]any{"exp": exp.Unix()})
	return "header." + base64.RawURLEncoding.EncodeToString(data) + ".signature"
}

func TestCodexProviderValidationAndRouting(t *testing.T) {
	l := localSetup{Providers: []provider{{Name: "codex", Type: "codex"}}, Models: []localModel{{Provider: "codex", Model: "gpt-test"}}}
	if err := l.validate(); err != nil {
		t.Fatal(err)
	}
	if l.Providers[0].BaseURL != codexBaseURL {
		t.Fatal("Codex endpoint not pinned")
	}
	c := config{local: l}
	if c.routeFor("codex/gpt-test", "default").Mode != "disabled" {
		t.Fatal("backend bypasses explicit routes")
	}
	c.local.ModelPools = map[string][]poolTarget{"codex": {{Model: "codex/gpt-test", Effort: "high"}}}
	c.local.Routes = map[string]map[string]modelRoute{"claude-opus-5": {"high": {Mode: "pool", Pool: "codex"}}}
	if len(c.forModel("claude-opus-5", "high").local.Models) != 1 {
		t.Fatal("Codex pool missing")
	}
	l.Providers[0].BaseURL = "https://other.test"
	if err := l.validate(); err == nil {
		t.Fatal("accepted alternate OAuth endpoint")
	}
}

func TestIncomingNameCannotOverridePoolOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q},"finish_reason":"stop"}]}`, body.Model)
	}))
	defer server.Close()
	l := localSetup{Providers: []provider{{Name: "p", BaseURL: server.URL}}, Models: []localModel{{Provider: "p", Model: "a"}, {Provider: "p", Model: "b"}}, Preferred: "p/a"}
	cfg := config{local: l, failover: false}
	request := httptest.NewRequest("POST", "/v1/messages", nil)
	w := httptest.NewRecorder()
	handleLocal(w, request, cfg, []byte(`{"model":"p/b","messages":[{"role":"user","content":"hi"}]}`), nil, newHealth(""))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"text":"a"`) {
		t.Fatalf("wrong model: %d %s", w.Code, w.Body.String())
	}
}

func TestCodexBrowserLogin(t *testing.T) {
	var verifier string
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Errorf("token path: %s", r.URL.Path)
		}
		verifier = r.FormValue("code_verifier")
		if r.FormValue("grant_type") != "authorization_code" || r.FormValue("code") != "one-time-code" || verifier == "" {
			t.Error("bad code exchange")
		}
		idData, _ := json.Marshal(map[string]any{"chatgpt_account_id": "account-browser"})
		idToken := "header." + base64.RawURLEncoding.EncodeToString(idData) + ".signature"
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"refresh-browser","id_token":%q}`, testJWT(time.Now().Add(time.Hour)), idToken)
	}))
	defer issuer.Close()
	flow, err := startCodexBrowserFlow(t.Context(), "127.0.0.1:0", issuer.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer flow.server.Close()
	loginURL, _ := url.Parse(flow.URL)
	if loginURL.Query().Get("code_challenge_method") != "S256" || loginURL.Query().Get("redirect_uri") != flow.redirectURI {
		t.Fatal("bad authorization URL")
	}
	s := &codexAuthStore{path: filepath.Join(t.TempDir(), "auth.json"), issuer: issuer.URL, client: issuer.Client()}
	done := make(chan error, 1)
	go func() { done <- s.finishBrowserFlow(t.Context(), flow) }()
	resp, err := http.Get(flow.redirectURI + "?code=one-time-code&state=" + url.QueryEscape(flow.state))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("callback: HTTP %d", resp.StatusCode)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if verifier != flow.verifier {
		t.Fatal("PKCE verifier mismatch")
	}
	if c, err := readCodexCredential(s.path); err != nil || c.Tokens.AccountID != "account-browser" {
		t.Fatalf("stored credential: %v", err)
	}
}

func TestCodexBrowserRejectsWrongState(t *testing.T) {
	flow, err := startCodexBrowserFlow(t.Context(), "127.0.0.1:0", "https://auth.openai.com")
	if err != nil {
		t.Fatal(err)
	}
	defer flow.server.Close()
	resp, err := http.Get(flow.redirectURI + "?code=one-time-code&state=wrong")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad state: HTTP %d", resp.StatusCode)
	}
	result := <-flow.result
	if result.err == nil || result.code != "" {
		t.Fatal("unverified code accepted")
	}
}
