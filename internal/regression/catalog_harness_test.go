package regression_test

import (
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
	"sync"
	"testing"
	"time"
)

func newCatalogStub(t *testing.T, name, expectedURI, contentType, body string, journal *startupJournal) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		journal.record(name, r.Method, r.URL.RequestURI())
		if r.Method != http.MethodGet || r.URL.RequestURI() != expectedURI {
			http.Error(w, "unexpected synthetic catalog request", http.StatusNotFound)
			return
		}
		if name == "codex" && (r.Header.Get("ChatGPT-Account-Id") != "catalogsynthetic-account" ||
			r.Header.Get("originator") != "claude-router" ||
			!strings.HasPrefix(r.Header.Get("Authorization"), "Bearer catalogsynthetic.")) {
			http.Error(w, "synthetic catalog credential required", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	if err := validateLoopback(server.URL); err != nil {
		t.Fatal(err)
	}
	return server
}

type startupProbe struct {
	Target string
	Method string
	URI    string
}

type startupJournal struct {
	mu     sync.Mutex
	probes []startupProbe
}

func (j *startupJournal) record(target, method, uri string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.probes = append(j.probes, startupProbe{Target: target, Method: method, URI: uri})
}

func (j *startupJournal) snapshot() []startupProbe {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]startupProbe(nil), j.probes...)
}

func compareStartupProbes(observed, expected []startupProbe) error {
	if len(observed) != len(expected) {
		return fmt.Errorf("catalog probes: %d observed, %d expected", len(observed), len(expected))
	}
	for i, want := range expected {
		if observed[i] != want {
			return fmt.Errorf("catalog probe %d: observed %+v, expected %+v", i, observed[i], want)
		}
	}
	return nil
}

type catalogFixture struct {
	root     string
	manifest string
	auth     string
}

type persistedCatalog struct {
	Anthropic        []string  `json:"anthropic"`
	AnthropicUpdated time.Time `json:"anthropic_updated"`
	CheckedAt        time.Time `json:"checked_at"`
	Providers        map[string]struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
		UpdatedAt time.Time `json:"updated_at"`
		Error     string    `json:"error"`
	} `json:"providers"`
	CodexSeen map[string][]string `json:"codex_seen"`
}

func (s *testStand) catalogState(t *testing.T) persistedCatalog {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(s.home, "providers.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Catalog persistedCatalog `json:"catalog"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatal(err)
	}
	return state.Catalog
}

type catalogProviderSpec struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	AuthID  string `json:"auth_id"`
}

func compareCatalogProviders(observed, expected []catalogProviderSpec) error {
	if len(observed) != len(expected) {
		return fmt.Errorf("catalog provider count: %d observed, %d expected", len(observed), len(expected))
	}
	for i, want := range expected {
		if observed[i] != want {
			return fmt.Errorf("catalog provider %d: observed %q, expected %q", i, observed[i].Name, want.Name)
		}
	}
	return nil
}

func (s *testStand) checkCatalogProviders() error {
	body, err := os.ReadFile(filepath.Join(s.home, "providers.json"))
	if err != nil {
		return err
	}
	var config struct {
		Providers []catalogProviderSpec `json:"providers"`
	}
	if err := json.Unmarshal(body, &config); err != nil {
		return err
	}
	return compareCatalogProviders(config.Providers, s.plannedProviders)
}

func (s *testStand) expectedCatalogProbes(t *testing.T) []startupProbe {
	t.Helper()
	if err := s.checkCatalogProviders(); err != nil {
		t.Fatalf("router mutated the immutable fixture provider list: %v", err)
	}
	want := []startupProbe{{Target: "official", Method: http.MethodGet, URI: "/overview"}}
	for _, provider := range s.plannedProviders {
		switch {
		case provider.Name == "codex" && provider.Type == "codex" && provider.BaseURL == "https://chatgpt.com/backend-api/codex":
			want = append(want, startupProbe{Target: "codex", Method: http.MethodGet, URI: "/models?client_version=0.159.2"})
		case provider.Name == "fixture-a" && provider.Type == "" && provider.BaseURL == s.a.server.URL+"/v1":
			want = append(want, startupProbe{Target: "fixture-a", Method: http.MethodGet, URI: "/v1/models"})
		case provider.Name == "fixture-b" && provider.Type == "" && provider.BaseURL == s.b.server.URL+"/v1":
			want = append(want, startupProbe{Target: "fixture-b", Method: http.MethodGet, URI: "/v1/models"})
		default:
			t.Fatalf("blocked: unapproved catalog provider %q in fixture", provider.Name)
		}
	}
	if len(want) < 2 {
		t.Fatal("synthetic catalog fixture must probe at least one local provider")
	}
	return want
}

func (s *testStand) awaitCatalog(t *testing.T, prior time.Time, before int, want []startupProbe) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		observed := s.probes.snapshot()
		if len(observed) > before+len(want) {
			t.Fatalf("unlisted synthetic catalog probe: %v", observed[before:])
		}
		if len(observed) == before+len(want) {
			if err := compareStartupProbes(observed[before:], want); err != nil {
				t.Fatal(err)
			}
			catalog := s.catalogState(t)
			if catalog.CheckedAt.After(prior) {
				if err := s.checkCatalogProviders(); err != nil {
					t.Fatalf("router mutated the immutable fixture provider list after catalog persistence: %v", err)
				}
				if len(catalog.Anthropic) != 1 || catalog.Anthropic[0] != "claude-opus-5-5" || catalog.AnthropicUpdated.IsZero() {
					t.Fatal("synthetic official catalog was not parsed and persisted")
				}
				if len(catalog.Providers) != len(want)-1 {
					t.Fatal("synthetic catalog contains an unlisted provider")
				}
				models := map[string]string{
					"fixture-a": "fixture-a-model", "fixture-b": "fixture-b-model", "codex": "gpt-6-sol",
				}
				codexExpected := false
				for _, probe := range want[1:] {
					provider := catalog.Providers[probe.Target]
					if provider.UpdatedAt.IsZero() || provider.Error != "" || len(provider.Models) != 1 ||
						provider.Models[0].ID != models[probe.Target] {
						t.Fatalf("synthetic provider %q was not parsed and persisted", probe.Target)
					}
					codexExpected = codexExpected || probe.Target == "codex"
				}
				if codexExpected {
					seen := catalog.CodexSeen["codex"]
					if len(catalog.CodexSeen) != 1 || len(seen) != 1 || seen[0] != "gpt-6-sol" {
						t.Fatal("synthetic Codex discovery was not persisted")
					}
				} else if len(catalog.CodexSeen) != 0 {
					t.Fatal("unlisted Codex discovery was persisted")
				}
				s.expected = append(s.expected, want...)
				return
			}
		}
		select {
		case <-s.waited:
			t.Fatalf("router exited before catalog persistence: %v", s.exitErr)
		case <-deadline.C:
			t.Fatalf("blocked: synthetic catalog refresh did not persist: %d/%d probes", len(observed)-before, len(want))
		case <-ticker.C:
		}
	}
}

func (s *testStand) manualCatalogRefresh(t *testing.T) {
	t.Helper()
	prior := s.catalogState(t).CheckedAt
	before := len(s.probes.snapshot())
	want := s.expectedCatalogProbes(t)
	status, _ := s.uiCall(t, http.MethodPost, "/settings/refresh-models", []byte{}, s.uiURL)
	if status != http.StatusOK {
		t.Fatalf("RR-CAT-01: manual refresh returned HTTP %d", status)
	}
	s.awaitCatalog(t, prior, before, want)
}

func writeCatalogFixture(t *testing.T, root, official, codex string, origins []string) catalogFixture {
	t.Helper()
	if err := validateLoopback(official); err != nil {
		t.Fatal(err)
	}
	codeURL, err := url.Parse(codex)
	if err != nil || codeURL.Path != "/models" || codeURL.RawQuery != "client_version=0.159.2" {
		t.Fatal("synthetic Codex models URL is not canonical")
	}
	if err := validateLoopback(codeURL.Scheme + "://" + codeURL.Host); err != nil {
		t.Fatal(err)
	}
	for _, origin := range origins {
		if err := validateLoopback(origin); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := json.Marshal(map[string]int64{"exp": time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	token := "catalogsynthetic." + base64.RawURLEncoding.EncodeToString(claims) + ".fixture"
	auth, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt", "tokens": map[string]string{
			"access_token": token, "refresh_token": "catalogsynthetic-refresh", "account_id": "catalogsynthetic-account",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := catalogFixture{root: canonical, manifest: filepath.Join(canonical, "catalog.json"), auth: filepath.Join(canonical, "codex-auth.json")}
	if err := os.WriteFile(fixture.auth, auth, 0600); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"fixture_root": canonical, "official_url": official,
		"codex_models_url": codex, "provider_origins": origins,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.manifest, body, 0600); err != nil {
		t.Fatal(err)
	}
	return fixture
}
