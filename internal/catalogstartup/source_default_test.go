//go:build !catalogsynthetic

package catalogstartup

import "testing"

func TestProductionBuildIgnoresSyntheticManifest(t *testing.T) {
	t.Setenv("ROUTER_CATALOG_SYNTHETIC_MANIFEST", "/not/a/manifest")
	deps, err := ForProcess("https://platform.claude.com/docs/en/models/overview", "https://chatgpt.com/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	defer deps.Close()
	if deps.OfficialURL() != "https://platform.claude.com/docs/en/models/overview" {
		t.Fatalf("official URL = %s", deps.OfficialURL())
	}
	if err := deps.ValidateProvider("p", "openai", "https://example.com/v1"); err != nil {
		t.Fatalf("production provider rejected: %v", err)
	}
}
