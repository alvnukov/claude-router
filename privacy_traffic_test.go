package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"localrouter/internal/history"
	"localrouter/internal/privacy"
)

const trafficCanary = "Canary-credential-123"

func trafficFixture(t *testing.T, up *httptest.Server, local bool) (http.Handler, *history.Store, string) {
	t.Helper()
	home := t.TempDir()
	base, _ := url.Parse(up.URL)
	cfg := config{upstream: base, firstByte: time.Second, failover: true, local: oneProvider(up.URL, "good")}
	route := modelRoute{Mode: "anthropic"}
	if local {
		route = modelRoute{Mode: "model", Model: "p/good"}
	}
	cfg.local.Routes = map[string]map[string]modelRoute{"test": {"default": route}}
	cs := newConfigStore(cfg, filepath.Join(home, "providers.json"))
	st := history.New(10, filepath.Join(home, "history.jsonl"))
	hl := newHealth("")
	writeTrafficConfig(t, home, `{}`)
	return newMainHandler(cfg, cs, st, hl, newUIServer(st, cs, hl)), st, home
}
func writeTrafficConfig(t *testing.T, home, rules string) {
	t.Helper()
	b := `{"version":1,"enabled":true,"default":"safe","profiles":[{"id":"safe","name":"Safe","enabled":true,"rules":` + rules + `}],"bindings":[]}`
	if err := os.WriteFile(filepath.Join(home, "privacy-profiles.json"), []byte(b), 0600); err != nil {
		t.Fatal(err)
	}
}
func trafficCall(h http.Handler, path, body string) *httptest.ResponseRecorder {
	server := httptest.NewServer(h)
	defer server.Close()
	w := httptest.NewRecorder() // result container; the handler sees a real HTTP writer
	r, err := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(body))
	if err != nil {
		w.Code = 599
		w.Body.WriteString(err.Error())
		return w
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Api-Key", "synthetic-client-key")
	r.Header.Set("X-Leak", trafficCanary)
	r.Header.Set("Cookie", trafficCanary)
	r.Header.Set("User-Agent", trafficCanary)
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	resp, err := client.Do(r)
	if err != nil {
		w.Code = 599
		w.Body.WriteString(err.Error())
		return w
	}
	defer resp.Body.Close()
	for name, values := range resp.Header {
		w.Header()[name] = append([]string(nil), values...)
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w.Body, resp.Body); err != nil {
		w.Code = 599
		w.Body.WriteString(err.Error())
	}
	return w
}

const trafficBody = `{"model":"test","max_tokens":100,"metadata":{"user_id":"{\"session_id\":\"private-session\",\"email\":\"Canary-credential-123\"}"},"messages":[{"role":"user","content":"password=Canary-credential-123"}]}`

func TestPrivacyTrafficAnthropicAndOpenAI(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(map[bool]string{false: "anthropic", true: "openai"}[local], func(t *testing.T) {
			var seen atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen.Add(1)
				b, _ := io.ReadAll(r.Body)
				var payload, original map[string]json.RawMessage
				if err := json.Unmarshal(b, &payload); err != nil {
					t.Error(err)
				}
				_ = json.Unmarshal([]byte(trafficBody), &original)
				if bytes.Contains(payload["messages"], []byte(trafficCanary)) {
					t.Error("supported text leaked")
				}
				if !local && !bytes.Equal(payload["metadata"], original["metadata"]) {
					t.Error("service metadata changed")
				}
				if strings.Contains(fmtHeader(r.Header), trafficCanary) {
					t.Error("header leak")
				}
				var req struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				_ = json.Unmarshal(b, &req)
				text := req.Messages[len(req.Messages)-1].Content
				w.Header().Set("Content-Type", "application/json")
				if local {
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": text}, "finish_reason": "stop"}}})
				} else {
					_ = json.NewEncoder(w).Encode(map[string]any{"type": "message", "content": []any{map[string]string{"type": "text", "text": text}}})
				}
			}))
			defer up.Close()
			h, st, home := trafficFixture(t, up, local)
			w := trafficCall(h, "/v1/messages", trafficBody)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), trafficCanary) || seen.Load() != 1 {
				t.Fatalf("code=%d response=%s calls=%d", w.Code, w.Body.String(), seen.Load())
			}
			if len(st.List()) != 0 {
				t.Fatal("protected traffic recorded in history")
			}
			if b, _ := os.ReadFile(filepath.Join(home, "history.jsonl")); bytes.Contains(b, []byte(trafficCanary)) {
				t.Fatal("history leak")
			}
		})
	}
}
func fmtHeader(h http.Header) string { b, _ := json.Marshal(h); return string(b) }

func TestPrivacyTrafficDetectOnlyPreservesOriginalData(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(map[bool]string{false: "anthropic", true: "openai"}[local], func(t *testing.T) {
			var calls atomic.Int32
			const responseText = "password=" + trafficCanary + " <secret:credential:deadbeef>"
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				b, _ := io.ReadAll(r.Body)
				if !bytes.Contains(b, []byte(trafficCanary)) {
					t.Error("detect-only masked request")
				}
				if !local && string(b) != trafficBody {
					t.Error("direct detect-only request bytes changed")
				}
				w.Header().Set("Content-Type", "application/json")
				if local {
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": responseText}, "finish_reason": "stop"}}})
				} else {
					_ = json.NewEncoder(w).Encode(map[string]any{"content": []any{map[string]any{"type": "text", "text": responseText}}})
				}
			}))
			defer up.Close()
			h, history, home := trafficFixture(t, up, local)
			path := filepath.Join(home, "privacy-profiles.json")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			b = bytes.Replace(b, []byte(`"rules":`), []byte(`"mode":"detect","rules":`), 1)
			if err = os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			w := trafficCall(h, "/v1/messages", trafficBody)
			var response struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(w.Body.Bytes(), &response) != nil || w.Code != 200 || len(response.Content) != 1 || response.Content[0].Text != responseText || calls.Load() != 1 {
				t.Fatalf("detect output altered: status=%d body=%s", w.Code, w.Body)
			}
			if len(history.List()) != 0 {
				t.Fatal("detect-only retained input in history")
			}
			if _, err := os.Stat(filepath.Join(home, "privacy-runtime")); !os.IsNotExist(err) {
				t.Fatal("detect-only created dictionaries")
			}
		})
	}
}

func TestPrivacyTrafficFailClosedRoutesAndConfig(t *testing.T) {
	var seen atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":"ok"}`)
	}))
	defer up.Close()
	h, st, home := trafficFixture(t, up, false)
	for _, path := range []string{"/unknown/" + trafficCanary, "/v1/messages?secret=" + trafficCanary} {
		if w := trafficCall(h, path, trafficBody); w.Code < 400 {
			t.Fatal("unsafe route accepted")
		}
	}
	if w := trafficCall(h, "/v1/messages", `{"model":"test","system":"safe","unknown":"`+trafficCanary+`"}`); w.Code != http.StatusOK {
		t.Fatal("unknown field blocked supported text")
	}
	if seen.Load() != 1 || len(st.List()) != 0 {
		t.Fatal("rejected input escaped")
	}
	if err := os.WriteFile(filepath.Join(home, "privacy-profiles.json"), []byte(`{"enabled":false,`), 0600); err != nil {
		t.Fatal(err)
	}
	if w := trafficCall(h, "/v1/messages", trafficBody); w.Code < 400 {
		t.Fatal("invalid policy bypass")
	}
	if err := os.Remove(filepath.Join(home, "privacy-profiles.json")); err != nil {
		t.Fatal(err)
	}
	if w := trafficCall(h, "/v1/messages", trafficBody); w.Code < 400 {
		t.Fatal("deleted policy bypass")
	}
	if seen.Load() != 1 {
		t.Fatal("sent after configuration failure")
	}
}
func TestPrivacyTrafficCountTokensAndErrors(t *testing.T) {
	var count atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if bytes.Contains(b, []byte(trafficCanary)) {
			t.Error("count tokens leak")
		}
		count.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "count_tokens") {
			_, _ = io.WriteString(w, `{"input_tokens":12}`)
		} else {
			w.WriteHeader(500)
			_, _ = io.WriteString(w, trafficCanary)
		}
	}))
	defer up.Close()
	h, _, _ := trafficFixture(t, up, true)
	countResponse := trafficCall(h, "/v1/messages/count_tokens", trafficBody)
	var estimate struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(countResponse.Body.Bytes(), &estimate); err != nil {
		t.Fatal(err)
	}
	if countResponse.Code != http.StatusOK || estimate.InputTokens != len(trafficBody)/4 || count.Load() != 0 {
		t.Fatalf("local count failed: %d %s", countResponse.Code, countResponse.Body.String())
	}
	if w := trafficCall(h, "/v1/messages", trafficBody); w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), trafficCanary) {
		t.Fatal("unsafe local upstream error")
	}
	if count.Load() != 1 {
		t.Fatal("unsupported count reached provider or local error was not sent")
	}
}
func TestPrivacyTrafficRejectsMalformedToolWithoutRepair(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"tool_calls":[{"id":"call","type":"function","function":{"name":"run","arguments":"{broken"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer up.Close()
	h, _, _ := trafficFixture(t, up, true)
	w := trafficCall(h, "/v1/messages", trafficBody)
	if w.Code < 400 || strings.Contains(w.Body.String(), `"tool_use"`) {
		t.Fatal("malformed tool call repaired or released")
	}
}

func TestPrivacyTrafficFailoverUsesActualTargetAndFrozenPolicy(t *testing.T) {
	home := t.TempDir()
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, string(b))
		w.Header().Set("Content-Type", "application/json")
		if len(seen) == 1 {
			if err := os.WriteFile(filepath.Join(home, "privacy-profiles.json"), []byte(`{"version":1,"enabled":false,"profiles":[],"bindings":[]}`), 0600); err != nil {
				t.Error(err)
			}
			w.WriteHeader(500)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer up.Close()
	base, _ := url.Parse(up.URL)
	cfg := config{upstream: base, firstByte: time.Second, failover: true, local: oneProvider(up.URL, "a", "b")}
	cfg.local.Routes = map[string]map[string]modelRoute{"test": {"default": {Mode: "pool", Pool: "both"}}}
	cfg.local.ModelPools = map[string][]poolTarget{"both": {{Model: "p/a"}, {Model: "p/b"}}}
	setup := `{"version":1,"enabled":true,"default":"a","profiles":[{"id":"a","name":"A","enabled":true,"rules":{"entries":[{"kind":"org","forms":["AlphaCanary"]}]}},{"id":"b","name":"B","enabled":true,"rules":{"entries":[{"kind":"org","forms":["BetaCanary"]}]}}],"bindings":[{"kind":"model","target":"p/b","profile":"b"}]}`
	if err := os.WriteFile(filepath.Join(home, "privacy-profiles.json"), []byte(setup), 0600); err != nil {
		t.Fatal(err)
	}
	cs := newConfigStore(cfg, filepath.Join(home, "providers.json"))
	st := history.New(10, "")
	hl := newHealth("")
	h := newMainHandler(cfg, cs, st, hl, newUIServer(st, cs, hl))
	w := trafficCall(h, "/v1/messages", `{"model":"test","messages":[{"role":"user","content":"AlphaCanary BetaCanary"}]}`)
	if w.Code != 200 || len(seen) != 2 {
		t.Fatalf("failover %d: %s (%d attempts)", w.Code, w.Body.String(), len(seen))
	}
	if strings.Contains(seen[0], "AlphaCanary") || !strings.Contains(seen[0], "BetaCanary") || !strings.Contains(seen[1], "AlphaCanary") || strings.Contains(seen[1], "BetaCanary") {
		t.Fatal("wrong per-attempt policy")
	}
}
func TestPrivacyTrafficSSEAtomic(t *testing.T) {
	for _, truncate := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "truncated"}[truncate], func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				var req struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				_ = json.Unmarshal(b, &req)
				text := req.Messages[0].Content
				w.Header().Set("Content-Type", "text/event-stream")
				for _, part := range []string{text[:len(text)/2], text[len(text)/2:]} {
					d, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": part}}}})
					_, _ = io.WriteString(w, "data: "+string(d)+"\n\n")
				}
				if !truncate {
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				}
			}))
			defer up.Close()
			h, _, _ := trafficFixture(t, up, true)
			w := trafficCall(h, "/v1/messages", strings.Replace(trafficBody, `"max_tokens":100`, `"max_tokens":100,"stream":true`, 1))
			if truncate {
				if w.Code < 400 || strings.Contains(w.Body.String(), trafficCanary) || strings.Contains(w.Body.String(), "content_block_start") {
					t.Fatal("partial stream released")
				}
			} else if w.Code != 200 || !strings.Contains(w.Body.String(), trafficCanary) {
				t.Fatalf("stream %d: %s", w.Code, w.Body.String())
			}
		})
	}
}
func TestPrivacyTrafficRedirectNeverFollowed(t *testing.T) {
	for _, local := range []bool{false, true} {
		var calls atomic.Int32
		dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
		defer dest.Close()
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, dest.URL, http.StatusTemporaryRedirect)
		}))
		defer up.Close()
		h, _, _ := trafficFixture(t, up, local)
		w := trafficCall(h, "/v1/messages", trafficBody)
		if w.Code < 400 || calls.Load() != 0 {
			t.Fatal("redirect followed")
		}
	}
}
func TestPrivacyTrafficUnknownNestedAndDuplicateArguments(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n <= 2 {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"tool_calls":[{"id":"call","type":"function","function":{"name":"run","arguments":"{\"cmd\":\"safe\",\"cmd\":\"dangerous\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer up.Close()
	h, _, _ := trafficFixture(t, up, true)
	for _, body := range []string{`{"model":"test","messages":[{"role":"user","content":[{"type":"text","text":"safe","unknown":"hidden content"}]}]}`, `{"model":"test","tools":[{"name":"search","type":"web_search_20250101"}],"messages":[{"role":"user","content":"safe"}]}`} {
		if w := trafficCall(h, "/v1/messages", body); w.Code != http.StatusOK {
			t.Fatalf("unknown structure blocked route: %d %s", w.Code, w.Body.String())
		}
	}
	if calls.Load() != 2 {
		t.Fatal("unknown input did not reach normal translator")
	}
	w := trafficCall(h, "/v1/messages", trafficBody)
	if w.Code < 400 || strings.Contains(w.Body.String(), "tool_use") {
		t.Fatal("duplicate arguments collapsed")
	}
}

func TestPrivacyTrafficCodexJSONAndStream(t *testing.T) {
	seedTwoConnections(t)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		if bytes.Contains(b, []byte(trafficCanary)) || bytes.Contains(b, []byte("private-session")) {
			t.Error("Codex input leaked")
		}
		var req struct {
			Input []struct {
				Content string `json:"content"`
			} `json:"input"`
		}
		if err := json.Unmarshal(b, &req); err != nil || len(req.Input) != 1 {
			t.Fatal("bad Codex input")
		}
		delta, _ := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": req.Input[0].Content})
		return usageResponse(200, "data: "+string(delta)+"\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-private\",\"status\":\"completed\"}}\n\n"), nil
	})
	for _, stream := range []bool{false, true} {
		home := t.TempDir()
		writeTrafficConfig(t, home, `{}`)
		base, _ := url.Parse(codexBaseURL)
		cfg := config{upstream: base, local: localSetup{Providers: []provider{{Name: "codex", Type: "codex", BaseURL: codexBaseURL}}, Models: []localModel{{Provider: "codex", Model: "good"}}, Routes: map[string]map[string]modelRoute{"test": {"default": {Mode: "model", Model: "codex/good"}}}}, firstByte: time.Second}
		cs := newConfigStore(cfg, filepath.Join(home, "providers.json"))
		st := history.New(10, "")
		hl := newHealth("")
		h := newMainHandler(cfg, cs, st, hl, newUIServer(st, cs, hl))
		body := trafficBody
		if stream {
			body = strings.Replace(body, `"max_tokens":100`, `"max_tokens":100,"stream":true`, 1)
		}
		w := trafficCall(h, "/v1/messages", body)
		if w.Code != 200 || !strings.Contains(w.Body.String(), trafficCanary) {
			t.Fatalf("Codex stream=%t status=%d response=%s", stream, w.Code, w.Body.String())
		}
	}
}
func TestPrivacyTrafficCapacityAndCancellation(t *testing.T) {
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":"done"}`)
	}))
	defer up.Close()
	defer close(release)
	h, _, _ := trafficFixture(t, up, true)
	server := httptest.NewServer(h)
	defer server.Close()
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{}, 4)
	for range 4 {
		go func() {
			defer func() { done <- struct{}{} }()
			r, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/messages", strings.NewReader(trafficBody))
			if err != nil {
				return
			}
			r.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(r)
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
	}
	for range 4 {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("requests not admitted")
		}
	}
	if w := trafficCall(h, "/v1/messages", trafficBody); w.Code != 503 {
		t.Fatal("capacity limit not enforced")
	}
	cancel()
	for range 4 {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("cancellation did not release request")
		}
	}
}
func TestPrivacyTrafficCountTokensPreservesExtraResponseFields(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":12,"content":"unexpected"}`)
	}))
	defer up.Close()
	h, _, _ := trafficFixture(t, up, false)
	if w := trafficCall(h, "/v1/messages/count_tokens", trafficBody); w.Code != http.StatusOK || w.Body.String() != `{"input_tokens":12,"content":"unexpected"}` {
		t.Fatal("valid count with extra fields changed or blocked")
	}
}

func TestPrivacyTrafficInvalidOpenAIStreamDoesNotRepair(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"safe prefix\"}}]}\n\ndata: {broken\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()
	h, _, _ := trafficFixture(t, up, true)
	body := strings.Replace(trafficBody, `"max_tokens":100`, `"max_tokens":100,"stream":true`, 1)
	w := trafficCall(h, "/v1/messages", body)
	if w.Code < 400 || strings.Contains(w.Body.String(), "safe prefix") {
		t.Fatal("damaged stream silently repaired")
	}
}

func TestPrivacyTrafficReviewProtocolHeaderLeak(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":"safe"}`)
	}))
	defer up.Close()
	h, _, home := trafficFixture(t, up, false)
	writeTrafficConfig(t, home, `{"entries":[{"kind":"org","forms":["PrivateCanary"]}]}`)
	for _, key := range []string{"Anthropic-Beta", "Anthropic-Version"} {
		r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(trafficBody))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(key, "PrivateCanary")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Error("sensitive protocol header accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("header sent upstream")
	}
}
func TestPrivacyTrafficReviewLateErrorDiagnosticsAndTrailingJSON(t *testing.T) {
	const token = "91827364554637281909182736455463728190918273645546"
	for _, payload := range []string{`{"choices":[{"message":{"content":"safe"}}],"usage":{"prompt_tokens":` + token + `}}`, `{"choices":[{"message":{"content":"safe"}}]} trailing-data`} {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, payload)
		}))
		home := t.TempDir()
		writeTrafficConfig(t, home, `{}`)
		base, _ := url.Parse(up.URL)
		cfg := config{upstream: base, local: oneProvider(up.URL, "good"), firstByte: time.Second}
		cfg.local.Routes = map[string]map[string]modelRoute{"test": {"default": {Mode: "model", Model: "p/good"}}}
		cs := newConfigStore(cfg, filepath.Join(home, "providers.json"))
		hl := newHealth("")
		st := history.New(10, "")
		h := newMainHandler(cfg, cs, st, hl, newUIServer(st, cs, hl))
		w := trafficCall(h, "/v1/messages", trafficBody)
		if w.Code < 400 {
			t.Error("invalid response released")
		}
		if strings.Contains(hl.snapshot("p/good").LastErr, token) {
			t.Error("response content entered health diagnostics")
		}
		up.Close()
	}
}

func TestPrivacyTrafficReviewOversizeWhitespace(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"safe"}}]}`)
		_, _ = io.WriteString(w, strings.Repeat(" ", privacy.TrafficOutputLimit))
	}))
	defer up.Close()
	h, _, _ := trafficFixture(t, up, true)
	if w := trafficCall(h, "/v1/messages", trafficBody); w.Code < 400 {
		t.Fatal("oversized upstream was silently truncated and accepted")
	}
}

type privacyBrokenStream struct{ err error }

func (r privacyBrokenStream) Read([]byte) (int, error) { return 0, r.err }
func TestPrivacyTrafficReviewStreamDiagnostics(t *testing.T) {
	var logs bytes.Buffer
	before := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(before)
	r := io.MultiReader(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"prefix\"}}]}\n\n"), privacyBrokenStream{errors.New("decode Codex event: " + trafficCanary)})
	if err := streamResponse(&privacyBuffer{header: make(http.Header)}, r, "test"); err == nil {
		t.Fatal("expected stream failure")
	}
	if strings.Contains(logs.String(), trafficCanary) {
		t.Fatal("stream diagnostics leaked upstream content")
	}
}
func TestPrivacyTrafficReviewActualModelAndHeaderSecrets(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		wire, _ := io.ReadAll(r.Body)
		if !bytes.Contains(wire, []byte(`"model":"good"`)) {
			t.Error("structural model identifier changed")
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer up.Close()
	h, _, home := trafficFixture(t, up, true)
	writeTrafficConfig(t, home, `{"entries":[{"kind":"org","forms":["good"]}]}`)
	if w := trafficCall(h, "/v1/messages", trafficBody); w.Code != http.StatusOK {
		t.Error("structural model identifier blocked request")
	}
	h, _, _ = trafficFixture(t, up, false)
	for _, value := range []string{trafficCanary, base64.StdEncoding.EncodeToString([]byte(trafficCanary))} {
		r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(trafficBody))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Anthropic-Beta", value)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Error("known request secret leaked through protocol header")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("invalid header request reached upstream")
	}
}
