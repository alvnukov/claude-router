package regression_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func action(t *testing.T, stand *testStand, name string, fields map[string]string) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"action": name, "fields": fields})
	if err != nil {
		t.Fatal(err)
	}
	return stand.uiCall(t, http.MethodPost, "/api/ui/actions", body, stand.uiURL)
}

func TestRegressionConfigCrashSafeRestart(t *testing.T) {
	stand := startRouter(t, testFixture{})
	file := filepath.Join(stand.home, "providers.json")
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	invalid := wireFixtureFrom(t, "state", "invalid-route.json")
	status, _ := stand.uiCall(t, http.MethodPost, "/api/ui/actions", invalid, stand.uiURL)
	if status != http.StatusBadRequest {
		t.Errorf("RR-CFG-01: invalid route save returned %d, want 400", status)
	}
	afterInvalid, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, afterInvalid) {
		t.Error("RR-CFG-01: invalid save changed providers.json")
	}
	stand.restart(t)
	status, body := stand.clientCall(t, "/v1/messages", requestWithEffort("claude-sonnet-4-5-20250929", "default", "config-fixture", false))
	if status != http.StatusOK {
		t.Fatalf("RR-CFG-01: restart after rejected save returned %d", status)
	}
	if err := checkRoutedMessage(body, "claude-sonnet-4-5-20250929", "OK-A"); err != nil {
		t.Error(err)
	}
	valid := wireFixtureFrom(t, "state", "valid-route.json")
	status, _ = stand.uiCall(t, http.MethodPost, "/api/ui/actions", valid, stand.uiURL)
	if status != http.StatusOK {
		t.Fatalf("RR-CFG-01: valid route save returned %d", status)
	}
	stand.restart(t)
	status, body = stand.clientCall(t, "/v1/messages", requestWithEffort("claude-sonnet-4-5-20250929", "default", "config-fixture", false))
	if status != http.StatusOK {
		t.Fatalf("RR-CFG-01: restart after valid save returned %d", status)
	}
	if err := checkRoutedMessage(body, "claude-sonnet-4-5-20250929", "OK-B"); err != nil {
		t.Error(err)
	}
	if len(stand.a.allCalls()) != 1 || len(stand.b.allCalls()) != 1 {
		t.Error("RR-CFG-01: old or new route missing from stub journal")
	}
}

func wireFixtureFrom(t *testing.T, section, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", section, name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestRegressionConfigOracleRejectsCorruptedResult(t *testing.T) {
	fixed := []byte(`{"active_profile":"rr-red","routes":{"claude-sonnet-4-5-20250929":{"high":{"mode":"pool","pool":"b"}}}}`)
	if err := compareJSON(fixed, fixed); err != nil {
		t.Fatal(err)
	}
	for _, changed := range [][]byte{
		[]byte(`{"active_profile":"rr-blue","routes":{"claude-sonnet-4-5-20250929":{"high":{"mode":"pool","pool":"b"}}}}`),
		[]byte(`{"active_profile":"rr-red","routes":{}}`),
		[]byte(`{"active_profile":"rr-red","routes":{"claude-sonnet-4-5-20250929":`),
	} {
		if err := compareJSON(changed, fixed); err == nil {
			t.Error("corrupted resulting configuration passed immutable oracle")
		}
	}
}

func TestRegressionProfileCloneAndDeletionGuards(t *testing.T) {
	stand := startRouter(t, testFixture{})
	if status, _ := action(t, stand, "profile.create", map[string]string{"name": "rr-clone", "mode": "clone"}); status != http.StatusOK {
		t.Fatalf("RR-PRO-02: clone status %d", status)
	}
	if status, _ := action(t, stand, "profile.delete", map[string]string{"name": "rr-red"}); status != http.StatusBadRequest {
		t.Errorf("RR-PRO-02: active profile deletion returned %d, want 400", status)
	}
	if status, _ := action(t, stand, "profile.activate", map[string]string{"name": "not-a-profile"}); status != http.StatusBadRequest {
		t.Errorf("RR-PRO-02: unknown profile activation returned %d, want 400", status)
	}
	stand.activateProfile(t, "rr-clone")
	status, body := stand.clientCall(t, "/v1/messages", requestWithEffort("claude-sonnet-4-5-20250929", "high", "clone-fixture", false))
	if status != http.StatusOK {
		t.Fatalf("RR-PRO-02: cloned route returned %d", status)
	}
	if err := checkRoutedMessage(body, "claude-sonnet-4-5-20250929", "OK-B"); err != nil {
		t.Error(err)
	}
	if len(stand.a.allCalls()) != 0 || len(stand.b.allCalls()) != 1 {
		t.Error("RR-PRO-02: cloned route chose the wrong upstream")
	}
}

func TestRegressionAccountIdentityAndNoCrossCall(t *testing.T) {
	// Existing Codex endpoints are hard-pinned and importing a real auth store
	// would exercise account credentials. Neither a local OpenAI stub nor a fake
	// status object proves refresh confinement or identity-preserving deletion.
	// Keep this mandatory RR-ACC-01/02/03 gap non-green until a reviewed safe
	// injection seam and an isolated B run are explicitly authorized.
	t.Error("blocked RR-ACC-01/02/03: no permitted full-path Codex auth/refresh injection seam; existing component suites require a separately reported result")
}

func TestRegressionInterceptionRoundTrip(t *testing.T) {
	stand := startRouter(t, testFixture{})
	settingsPath := filepath.Join(stand.home, "claude", "settings.json")
	seed := []byte(`{"theme":"fixture","env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:12345","FIXTURE_KEEP":"untouched"}}`)
	if err := os.WriteFile(settingsPath, seed, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(testBinary(t), "env", "-home", stand.home)
	cmd.Dir = stand.home
	cmd.Env = append([]string(nil), stand.env...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("RR-INT-01: isolated env command exited: %v (%d output bytes)", err, len(output))
	}
	if !bytes.Contains(output, []byte("ANTHROPIC_BASE_URL="+stand.apiURL)) {
		t.Error("RR-INT-01: env command did not select the test listener")
	}
	updated, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(updated, &value); err != nil {
		t.Fatal(err)
	}
	env, ok := value["env"].(map[string]any)
	if !ok || env["ANTHROPIC_BASE_URL"] != stand.apiURL || env["FIXTURE_KEEP"] != "untouched" || value["theme"] != "fixture" {
		t.Errorf("RR-INT-01: endpoint or unrelated setting lost (endpoint matches: %v)", env["ANTHROPIC_BASE_URL"] == stand.apiURL)
	}
	if _, err := os.Stat(settingsPath + ".router-proxy-backup"); err != nil {
		t.Error("RR-INT-01: no private restore record")
	}
	status, _ := action(t, stand, "interception.set", map[string]string{"op": "restore"})
	if status != http.StatusOK {
		t.Errorf("RR-INT-01: restore action returned %d", status)
	}
	restored, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := compareJSON(restored, seed); err != nil {
		t.Errorf("RR-INT-01: restore did not preserve original settings: %v", err)
	}
	if _, err := os.Stat(settingsPath + ".router-proxy-backup"); !os.IsNotExist(err) {
		t.Errorf("RR-INT-01: backup still exists after restore: %v", err)
	}
}

func TestRegressionInterceptionRejectsExternalConflict(t *testing.T) {
	stand := startRouter(t, testFixture{})
	settingsPath := filepath.Join(stand.home, "claude", "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:12345"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(testBinary(t), "env", "-home", stand.home)
	cmd.Dir, cmd.Env = stand.home, append([]string(nil), stand.env...)
	if _, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err)
	}
	userChanged := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:23456","FIXTURE_KEEP":"new"}}`)
	if err := os.WriteFile(settingsPath, userChanged, 0600); err != nil {
		t.Fatal(err)
	}
	status, _ := action(t, stand, "interception.set", map[string]string{"op": "restore"})
	if status != http.StatusBadRequest {
		t.Errorf("RR-INT-01: conflicting restore returned %d, want explicit error", status)
	}
	after, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, userChanged) {
		t.Error("RR-INT-01: external edit overwritten during refused restore")
	}
}

func TestRegressionCatalogRollback(t *testing.T) {
	stand := startRouter(t, testFixture{})
	file := filepath.Join(stand.home, "providers.json")
	status, _ := action(t, stand, "model.edit", map[string]string{
		"op": "add", "provider": "fixture-a", "model": "fixture-a-model",
	})
	if status != http.StatusOK {
		t.Errorf("RR-CAT-01: idempotent duplicate add returned %d, want 200", status)
	}
	duplicate, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Models []struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"models"`
	}
	if err := json.Unmarshal(duplicate, &catalog); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, model := range catalog.Models {
		if model.Provider == "fixture-a" && model.Model == "fixture-a-model" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("RR-CAT-01: duplicate add left %d identical models, want one", count)
	}
	status, _ = action(t, stand, "model.edit", map[string]string{
		"op": "add", "provider": "fixture-a", "model": `invalid"model`,
	})
	if status != http.StatusBadRequest {
		t.Errorf("RR-CAT-01: invalid model ID returned %d, want 400", status)
	}
	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(duplicate, after) {
		t.Error("RR-CAT-01: rejected invalid ID changed catalog/provider file")
	}
	stand.restart(t)
	status, body := stand.clientCall(t, "/v1/messages", requestWithEffort("claude-sonnet-4-5-20250929", "default", "catalog-fixture", false))
	if status != http.StatusOK {
		t.Fatalf("RR-CAT-01: original route vanished after restart: status %d", status)
	}
	if err := checkRoutedMessage(body, "claude-sonnet-4-5-20250929", "OK-A"); err != nil {
		t.Error(err)
	}
	if len(stand.b.allCalls()) != 0 {
		t.Error("RR-CAT-01: duplicate model changed destination")
	}
	// catalog.refresh also queries the hardcoded Anthropic catalog. Without an
	// explicit safe seam, running it would cross the test-only egress boundary.
	t.Error("blocked RR-CAT-01 full refresh: hardcoded Anthropic catalog endpoint has no safe local stub; duplicate rollback alone is partial evidence")
}
