//go:build catalogsynthetic && !router_codex_loopback

package catalogstartup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestChatGPTPlanSyntheticPinsAndFixtureAuthorization(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer catalogsynthetic-plan-access" || r.Header.Get("ChatGPT-Account-Id") != "" {
			t.Error("invalid mapped plan request")
		}
		w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	root := canonicalFixtureRoot(t)
	path := writeSyntheticManifest(t, root, srv.URL+"/overview", srv.URL+"/models?client_version=0.156.0", nil)
	raw, _ := os.ReadFile(path)
	var m map[string]any
	json.Unmarshal(raw, &m)
	m["chatgpt_plan_models_url"] = srv.URL + "/v1/models"
	raw, _ = json.Marshal(m)
	os.WriteFile(path, raw, 0600)
	auth := map[string]any{"auth_mode": "chatgpt-plan", "tokens": map[string]string{"access_token": "catalogsynthetic-plan-access", "refresh_token": "catalogsynthetic-refresh", "id_token": "catalogsynthetic-plan-id"}, "plan": map[string]any{"issuer": "https://auth.openai.com", "subject": "catalogsynthetic-account", "client_id": "catalogsynthetic-client", "host_id": "urn:uuid:11111111-1111-4111-8111-111111111111", "expires_at": time.Now().Add(time.Hour), "scopes": []string{"chatgpt.tokens.use.direct"}}}
	raw, _ = json.Marshal(auth)
	os.WriteFile(os.Getenv("ROUTER_CODEX_AUTH_FILE"), raw, 0600)
	t.Setenv(syntheticManifestEnv, path)
	deps, err := ForProcess("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer deps.Close()
	for _, token := range []string{"catalogsynthetic-plan-access", "production-looking-token"} {
		body, msg := deps.FetchModels(context.Background(), "", ProbeInput{Kind: "codex", Name: "codex", ChatGPTPlan: true}, func(_ context.Context, r *http.Request) error {
			if r.URL.String() != "https://api.openai.com/v1/models" {
				t.Error("authorized unpinned request")
			}
			r.Header.Set("Authorization", "Bearer "+token)
			return nil
		})
		if token == "catalogsynthetic-plan-access" {
			if msg != "" || string(body) != `{"models":[]}` {
				t.Fatalf("public fixture fetch %s", msg)
			}
		} else if msg == "" {
			t.Fatal("production token reached fixture")
		}
	}
	if hits != 1 {
		t.Fatalf("unapproved traffic: %d requests", hits)
	}
}
