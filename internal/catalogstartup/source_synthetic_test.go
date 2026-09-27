//go:build catalogsynthetic

package catalogstartup

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func canonicalFixtureRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func writeSyntheticManifest(t *testing.T, root, official, codex string, origins []string) string {
	t.Helper()
	authPath := filepath.Join(root, "codex-auth.json")
	claims, err := json.Marshal(map[string]int64{"exp": time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	token := "catalogsynthetic." + base64.RawURLEncoding.EncodeToString(claims) + ".fixture"
	credential, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt", "tokens": map[string]string{
			"access_token": token, "refresh_token": "catalogsynthetic-refresh", "account_id": "catalogsynthetic-account",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authPath, credential, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROUTER_CODEX_AUTH_FILE", authPath)
	path := filepath.Join(root, "catalog.json")
	body, err := json.Marshal(map[string]any{
		"fixture_root": root, "official_url": official,
		"codex_models_url": codex, "provider_origins": origins,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSyntheticCatalogFetchesApprovedEndpointsOnly(t *testing.T) {
	requests := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.String()
		_, _ = io.WriteString(w, "fixture")
	}))
	defer server.Close()
	root := canonicalFixtureRoot(t)
	t.Setenv("ROUTER_CATALOG_SYNTHETIC_MANIFEST", writeSyntheticManifest(t, root,
		server.URL+"/overview", server.URL+"/models?client_version=0.156.0", []string{server.URL}))
	deps, err := ForProcess("https://platform.claude.com/docs/en/models/overview", "https://chatgpt.com/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	defer deps.Close()
	if client := deps.SyntheticAuthClient(20 * time.Second); client == nil {
		t.Fatal("synthetic Codex credential refresh is not isolated")
	} else if resp, err := client.Get("https://auth.openai.com/oauth/token"); err == nil {
		resp.Body.Close()
		t.Fatal("synthetic Codex credential refresh reached an external issuer")
	}
	if deps.OfficialURL() != server.URL+"/overview" || deps.CodexModelsURL() != "https://chatgpt.com/backend-api/codex/models?client_version=0.156.0" {
		t.Fatal("official fixture or pinned Codex target not installed")
	}
	if err := deps.ValidateProvider("local", "openai", server.URL+"/v1"); err != nil {
		t.Fatal(err)
	}
	if err := deps.ValidateProvider("codex", "codex", "https://chatgpt.com/backend-api/codex"); err != nil {
		t.Fatal("Codex BaseURL should be ignored in favor of synthetic endpoint:", err)
	}
	if err := deps.ValidateProvider("second-account", "codex", "https://chatgpt.com/backend-api/codex"); err == nil {
		t.Fatal("synthetic Codex accepted a separate credential store")
	}
	if err := deps.ValidateProvider("codex", "codex", "https://chatgpt.com/backend-api/codex", "1234567890abcdef1234567890abcdef"); err == nil {
		t.Fatal("synthetic Codex accepted a non-legacy auth ID")
	}
	client := deps.Client(2 * time.Second)
	for _, target := range []string{deps.OfficialURL(), server.URL + "/v1/models"} {
		resp, err := client.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	credential, err := os.ReadFile(os.Getenv("ROUTER_CODEX_AUTH_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	var auth struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(credential, &auth); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, deps.CodexModelsURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+auth.Tokens.AccessToken)
	req.Header.Set("ChatGPT-Account-Id", "catalogsynthetic-account")
	req.Header.Set("originator", "claude-router")
	resp, err := deps.CodexClient(5 * time.Second).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, want := range []string{"/overview", "/v1/models", "/models?client_version=0.156.0"} {
		if got := <-requests; got != want {
			t.Fatalf("request = %q, want %q", got, want)
		}
	}
	for _, target := range []string{"https://example.com/models", "http://127.0.0.2:19876/models", server.URL + "/unexpected", server.URL + "/private/models", server.URL + "/models?client_version=0.156.0", deps.CodexModelsURL(), "https://chatgpt.com/backend-api/codex/usage"} {
		resp, err := client.Get(target)
		if resp != nil {
			resp.Body.Close()
		}
		if err == nil {
			t.Fatalf("unapproved catalog target accepted: %s", target)
		}
	}
	select {
	case got := <-requests:
		t.Fatalf("unapproved request reached server: %s", got)
	default:
	}
}

func TestSyntheticRejectsUnsafeManifestBeforeClientExists(t *testing.T) {
	root := canonicalFixtureRoot(t)
	base := "http://127.0.0.1:18881"
	valid := writeSyntheticManifest(t, root, base+"/overview", base+"/models?client_version=0.156.0", []string{base})
	badCases := []struct {
		name, content string
	}{
		{"external official", fmt.Sprintf(`{"fixture_root":%q,"official_url":"https://example.com/overview","codex_models_url":%q,"provider_origins":[%q]}`, root, base+"/models?client_version=0.156.0", base)},
		{"external codex", fmt.Sprintf(`{"fixture_root":%q,"official_url":%q,"codex_models_url":"https://chatgpt.com/backend-api/codex/models","provider_origins":[%q]}`, root, base+"/overview", base)},
		{"dns host", fmt.Sprintf(`{"fixture_root":%q,"official_url":"http://localhost:18881/overview","codex_models_url":%q,"provider_origins":[%q]}`, root, base+"/models?client_version=0.156.0", base)},
		{"external provider", fmt.Sprintf(`{"fixture_root":%q,"official_url":%q,"codex_models_url":%q,"provider_origins":["https://example.com"]}`, root, base+"/overview", base+"/models")},
		{"userinfo", fmt.Sprintf(`{"fixture_root":%q,"official_url":"http://user@127.0.0.1:18881/overview","codex_models_url":%q,"provider_origins":[%q]}`, root, base+"/models?client_version=0.156.0", base)},
		{"unknown field", fmt.Sprintf(`{"fixture_root":%q,"official_url":%q,"codex_models_url":%q,"provider_origins":[%q],"other":"no"}`, root, base+"/overview", base+"/models?client_version=0.156.0", base)},
		{"wrong root", fmt.Sprintf(`{"fixture_root":%q,"official_url":%q,"codex_models_url":%q,"provider_origins":[%q]}`, t.TempDir(), base+"/overview", base+"/models?client_version=0.156.0", base)},
		{"oversize", strings.Repeat(" ", 65537)},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(valid, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ROUTER_CATALOG_SYNTHETIC_MANIFEST", valid)
			if deps, err := ForProcess("prod", "prod"); err == nil {
				deps.Close()
				t.Fatal("unsafe manifest accepted")
			}
		})
	}
	t.Run("missing manifest", func(t *testing.T) {
		t.Setenv("ROUTER_CATALOG_SYNTHETIC_MANIFEST", "")
		if _, err := ForProcess("prod", "prod"); err == nil {
			t.Fatal("missing manifest accepted")
		}
	})
	t.Run("symlink ancestor", func(t *testing.T) {
		outer := canonicalFixtureRoot(t)
		real := filepath.Join(outer, "real")
		alias := filepath.Join(outer, "alias")
		if err := os.MkdirAll(filepath.Join(real, "child"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, alias); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(alias, "child")
		path := writeSyntheticManifest(t, root, base+"/overview", base+"/models?client_version=0.156.0", []string{base})
		t.Setenv("ROUTER_CATALOG_SYNTHETIC_MANIFEST", path)
		if deps, err := ForProcess("prod", "prod"); err == nil {
			deps.Close()
			t.Fatal("noncanonical symlink ancestor accepted")
		}
	})
	t.Run("manifest via alias parent", func(t *testing.T) {
		root := canonicalFixtureRoot(t)
		path := writeSyntheticManifest(t, root, base+"/overview", base+"/models?client_version=0.156.0", []string{base})
		alias := filepath.Join(canonicalFixtureRoot(t), "alias")
		if err := os.Symlink(root, alias); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ROUTER_CATALOG_SYNTHETIC_MANIFEST", filepath.Join(alias, filepath.Base(path)))
		if deps, err := ForProcess("prod", "prod"); err == nil {
			deps.Close()
			t.Fatal("manifest through alias parent accepted")
		}
	})
	t.Run("manifest symlink", func(t *testing.T) {
		path := filepath.Join(root, "catalog-link.json")
		if err := os.Symlink(valid, path); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ROUTER_CATALOG_SYNTHETIC_MANIFEST", path)
		if _, err := ForProcess("prod", "prod"); err == nil {
			t.Fatal("symlink manifest accepted")
		}
	})
}

func TestSyntheticRejectsNonFixtureCodexCredentials(t *testing.T) {
	root := canonicalFixtureRoot(t)
	base := "http://127.0.0.1:18881"
	manifest := writeSyntheticManifest(t, root, base+"/overview", base+"/models?client_version=0.156.0", []string{base})
	t.Setenv("ROUTER_CATALOG_SYNTHETIC_MANIFEST", manifest)
	authPath := os.Getenv("ROUTER_CODEX_AUTH_FILE")
	original, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if deps, err := ForProcess("prod", "prod"); err != nil {
		t.Fatalf("valid dummy credential rejected: %v", err)
	} else {
		deps.Close()
	}
	for _, tc := range []struct {
		name, path string
		content    []byte
	}{
		{"missing env", "", original},
		{"outside root", filepath.Join(t.TempDir(), "codex-auth.json"), original},
		{"real-looking token", authPath, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"real.header.sig","refresh_token":"real-refresh","account_id":"live"}}`)},
		{"empty file", authPath, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.path != "" {
				if err := os.WriteFile(tc.path, tc.content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("ROUTER_CODEX_AUTH_FILE", tc.path)
			if deps, err := ForProcess("prod", "prod"); err == nil {
				deps.Close()
				t.Fatal("non-fixture credential accepted")
			}
		})
	}
	t.Run("symlink auth file", func(t *testing.T) {
		if err := os.WriteFile(authPath, original, 0600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "codex-link.json")
		if err := os.Symlink(authPath, link); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ROUTER_CODEX_AUTH_FILE", link)
		if deps, err := ForProcess("prod", "prod"); err == nil {
			deps.Close()
			t.Fatal("symlink credential accepted")
		}
	})
}

func TestSyntheticRejectsNonApprovedProviderAndRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com/steal", http.StatusFound)
	}))
	defer server.Close()
	root := canonicalFixtureRoot(t)
	t.Setenv("ROUTER_CATALOG_SYNTHETIC_MANIFEST", writeSyntheticManifest(t, root,
		server.URL+"/overview", server.URL+"/models?client_version=0.156.0", []string{server.URL}))
	deps, err := ForProcess("prod", "prod")
	if err != nil {
		t.Fatal(err)
	}
	defer deps.Close()
	for _, base := range []string{"https://example.com", "http://localhost:18881", "http://127.0.0.1:18881", server.URL + "/v1?escape=1"} {
		if err := deps.ValidateProvider("untrusted", "openai", base); err == nil {
			t.Fatalf("unapproved provider %q accepted", base)
		}
	}
	_, err = deps.Client(time.Second).Get(deps.OfficialURL())
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("redirect not rejected: %v", err)
	}
}

func TestSyntheticCatalogRequestRespectsCancel(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	root := canonicalFixtureRoot(t)
	t.Setenv("ROUTER_CATALOG_SYNTHETIC_MANIFEST", writeSyntheticManifest(t, root,
		server.URL+"/overview", server.URL+"/models?client_version=0.156.0", []string{server.URL}))
	deps, err := ForProcess("prod", "prod")
	if err != nil {
		t.Fatal(err)
	}
	defer deps.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", deps.OfficialURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		resp, err := deps.Client(15 * time.Second).Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		finished <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not reach loopback fixture")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled request = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled catalog request did not finish")
	}
}
