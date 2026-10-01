package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	conf "localrouter/internal/config"
	"localrouter/internal/history"
	codexprovider "localrouter/internal/providers/codex"
)

func privateCodexHandler(t *testing.T) http.Handler {
	t.Helper()
	home := t.TempDir()
	writeTrafficConfig(t, home, `{}`)
	base, _ := url.Parse(conf.CodexBaseURL)
	cfg := config{Upstream: base, Local: localSetup{Providers: []provider{{Name: "codex", Type: "codex", BaseURL: conf.CodexBaseURL}}, Models: []localModel{{Provider: "codex", Model: "good"}}, Routes: map[string]map[string]modelRoute{"test": {"default": {Mode: "model", Model: "codex/good"}}}}, FirstByte: time.Second}
	cs := conf.NewStore(cfg, filepath.Join(home, "providers.json"))
	st, hl := history.New(10, ""), newHealth("")
	return newMainHandler(cfg, cs, st, hl, newUIServer(st, cs, hl))
}

func TestPrivacyCodexDoesNotImportOrPersistOpaqueState(t *testing.T) {
	testPrivacyCodexDoesNotImportOrPersistOpaqueState(t, false)
}
func TestChatGPTPlanPrivacyDoesNotImportOrPersistOpaqueState(t *testing.T) {
	testPrivacyCodexDoesNotImportOrPersistOpaqueState(t, true)
}
func testPrivacyCodexDoesNotImportOrPersistOpaqueState(t *testing.T, plan bool) {
	seedTwoConnections(t)
	p := provider{Name: "codex", Type: "codex"}
	auth, err := codexStoreFor(p)
	if err != nil {
		t.Fatal(err)
	}
	if plan {
		if err := auth.save(planFixture(t, "https://auth.openai.com", true, time.Now().Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	credential, err := auth.credentialFor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	scope := codexprovider.SessionKey("codex/good\x00" + credential.accountKey() + "\x00" + codexprovider.SessionKey("private-session"))
	state, err := codexprovider.Capture(scope, []byte(`{"model":"good","input":[{"type":"message","role":"user","content":"hi"}]}`), codexprovider.Completion{Calls: 1, ResponseIDs: []string{"legacy-response"}, Text: "Ready", Output: []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"` + trafficCanary + `"}`), json.RawMessage(`{"type":"message","role":"assistant","content":"Ready"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := codexprovider.NewReplayStore(filepath.Join(filepath.Dir(auth.path), "codex-state"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Save(context.Background(), scope, state); err != nil {
		t.Fatal(err)
	}
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		if bytes.Contains(b, []byte(trafficCanary)) || bytes.Contains(b, []byte("prompt_cache_key")) || r.Header.Get("session-id") != "" || r.Header.Get("x-codex-turn-state") != "" {
			t.Error("protected request imported opaque state or legacy session affinity")
		}
		return usageResponse(200, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"private-response\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Done\"}]}]}}\n\n"), nil
	})
	body := `{"model":"test","max_tokens":10,"metadata":{"user_id":"{\"session_id\":\"private-session\"}"},"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"Ready"},{"role":"user","content":"continue"}]}`
	w := trafficCall(privateCodexHandler(t), "/v1/messages", body)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	after, err := store.Load(context.Background(), scope)
	if err != nil || len(after) != 1 || !bytes.Equal(after[0], state) {
		t.Fatalf("protected request wrote native state: count=%d err=%v", len(after), err)
	}
}

func TestPrivacyCodexRejectsContinuationAndMalformedEvents(t *testing.T) {
	for name, response := range map[string]string{
		"continuation": `{"type":"response.completed","response":{"id":"r1","end_turn":false,"output":[{"type":"reasoning","encrypted_content":"opaque"}]}}`,
		"duplicate":    `{"type":"response.output_text.delta","delta":"first","delta":"second"}` + "\n\ndata: " + `{"type":"response.completed","response":{"id":"r1"}}`,
		"utf8":         "{\"type\":\"response.output_text.delta\",\"delta\":\"\xff\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\"}}",
	} {
		for _, stream := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/json", true: "/stream"}[stream], func(t *testing.T) {
				seedTwoConnections(t)
				old := http.DefaultTransport
				t.Cleanup(func() { http.DefaultTransport = old })
				calls := 0
				http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if calls > 1 {
						return usageResponse(200, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r2\"}}\n\n"), nil
					}
					return usageResponse(200, "data: "+response+"\n\n"), nil
				})
				body := trafficBody
				if stream {
					body = strings.Replace(body, `"max_tokens":100`, `"max_tokens":100,"stream":true`, 1)
				}
				w := trafficCall(privateCodexHandler(t), "/v1/messages", body)
				if w.Code != 502 || calls != 1 || strings.Contains(w.Body.String(), "message_start") {
					t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body)
				}
			})
		}
	}
}
