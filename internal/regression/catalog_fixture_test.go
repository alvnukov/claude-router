package regression_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRegressionStartupProbeOracleRejectsMissingAndUnlistedRequests(t *testing.T) {
	want := []startupProbe{
		{Target: "official", Method: "GET", URI: "/overview"},
		{Target: "fixture-a", Method: "GET", URI: "/v1/models"},
		{Target: "codex", Method: "GET", URI: "/models?client_version=0.156.0"},
	}
	journal := new(startupJournal)
	for _, probe := range want {
		journal.record(probe.Target, probe.Method, probe.URI)
	}
	if got := journal.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("startup journal = %v, want %v", got, want)
	}
	if err := compareStartupProbes(journal.snapshot(), want); err != nil {
		t.Fatal(err)
	}
	for _, changed := range [][]startupProbe{
		want[:2],
		append(append([]startupProbe(nil), want...), startupProbe{Target: "fixture-a", Method: "GET", URI: "/private/models"}),
		{{Target: "official", Method: "POST", URI: "/overview"}, want[1], want[2]},
	} {
		if err := compareStartupProbes(changed, want); err == nil {
			t.Fatal("corrupted startup journal passed fixed oracle")
		}
	}
}

func TestRegressionCatalogProbeJournalDoesNotPolluteRouteAttempts(t *testing.T) {
	stub := newUpstreamStub(t, []byte(`{"choices":[]}`))
	stub.name, stub.probes = "fixture-a", new(startupJournal)
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	response, err := client.Get(stub.server.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("synthetic model stub returned HTTP %d", response.StatusCode)
	}
	if err := compareStartupProbes(stub.probes.snapshot(), []startupProbe{{
		Target: "fixture-a", Method: http.MethodGet, URI: "/v1/models",
	}}); err != nil {
		t.Fatal(err)
	}
	if len(stub.allCalls()) != 0 {
		t.Fatal("catalog probe contaminated the route attempt journal")
	}
}

func TestRegressionPreflightRejectsOrdinaryHost(t *testing.T) {
	t.Setenv("ROUTER_TEST_SOURCE", "unapproved")
	if isolatedFullProcessTree() {
		t.Fatal("full-process preflight accepted an unapproved source")
	}
}

func TestRegressionSyntheticCodexRejectsExternalAccounts(t *testing.T) {
	for _, provider := range []map[string]string{
		{"name": "other", "type": "codex", "base_url": "https://chatgpt.com/backend-api/codex"},
		{"name": "codex", "type": "codex", "base_url": "https://example.invalid"},
		{"name": "codex", "type": "codex", "base_url": "https://chatgpt.com/backend-api/codex", "auth_id": "real-account"},
	} {
		if err := validateProviderFixture(map[string]any{"providers": []map[string]string{provider}}); err == nil {
			t.Fatalf("unapproved Codex provider accepted: %v", provider)
		}
	}
	if err := validateProviderFixture(map[string]any{"providers": []map[string]string{{
		"name": "codex", "type": "codex", "base_url": "https://chatgpt.com/backend-api/codex",
	}}}); err != nil {
		t.Fatal(err)
	}
}

func TestRegressionSyntheticCatalogFixtureHasPrivateDirectChildAuth(t *testing.T) {
	root := filepath.Join(t.TempDir(), "catalog-fixture")
	fixture := writeCatalogFixture(t, root, "http://127.0.0.1:19001/overview",
		"http://127.0.0.1:19002/models?client_version=0.156.0",
		[]string{"http://127.0.0.1:19003"})
	if filepath.Dir(fixture.manifest) != fixture.root || filepath.Dir(fixture.auth) != fixture.root {
		t.Fatal("catalog manifest or auth escaped fixture root")
	}
	if info, err := os.Lstat(fixture.auth); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("synthetic auth must be a private regular file: %v", err)
	}
	body, err := os.ReadFile(fixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		FixtureRoot     string   `json:"fixture_root"`
		OfficialURL     string   `json:"official_url"`
		CodexModelsURL  string   `json:"codex_models_url"`
		ProviderOrigins []string `json:"provider_origins"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.FixtureRoot != fixture.root || manifest.OfficialURL != "http://127.0.0.1:19001/overview" ||
		manifest.CodexModelsURL != "http://127.0.0.1:19002/models?client_version=0.156.0" ||
		!reflect.DeepEqual(manifest.ProviderOrigins, []string{"http://127.0.0.1:19003"}) {
		t.Fatal("synthetic catalog manifest differs from fixed fixture endpoints")
	}
}
