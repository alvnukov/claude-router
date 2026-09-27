//go:build catalogsynthetic && router_codex_loopback

package catalogstartup

import (
	"strings"
	"testing"
)

func TestCombinedSyntheticBuildRejectsBeforeManifestOrNetwork(t *testing.T) {
	t.Setenv("ROUTER_CATALOG_SYNTHETIC_MANIFEST", "")
	t.Setenv("ROUTER_CODEX_AUTH_FILE", "")
	deps, err := ForProcess("https://platform.claude.com/docs/en/models/overview", "https://chatgpt.com/backend-api/codex")
	if err == nil || !strings.Contains(err.Error(), "incompatible synthetic build tags") {
		deps.Close()
		t.Fatalf("combined build did not reject before reading the fixture: %v", err)
	}
	if deps.guard != nil || deps.transport != nil {
		t.Fatal("combined build returned an initialized network dependency")
	}
}
