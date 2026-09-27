package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"localrouter/internal/privacy"
)

const (
	uiOracleCanary = "RR-LAB-SYN-7319"
	uiOracleInput  = "password=RR-LAB-SYN-7319 keep-sentinel"
)

type privacyOracleBackend struct {
	*testBackend
	lab *privacy.Lab
}

func (b *privacyOracleBackend) PrivacyLab() *privacy.Lab { return b.lab }

func uiOracleCall(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var text string
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		text = string(encoded)
	}
	r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(text))
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

// The HTTP observer checks the executed handler as well as an independent
// hand-written canary and safe-text rule; nil/unexecuted is not a pass.
func uiOracleProtected(status int, body []byte) string {
	if status != http.StatusOK || len(body) == 0 {
		return "not-executed"
	}
	var result privacy.PreviewResult
	if json.Unmarshal(body, &result) != nil {
		return "invalid-response"
	}
	if result.Operation != privacy.ModeMask || !result.Enabled || !result.Roundtrip || result.ID == "" || result.Masked[privacy.KindSecret] < 1 || result.Output == "" {
		return "incomplete-preview"
	}
	if strings.Contains(string(body), uiOracleCanary) || strings.Contains(result.Output, uiOracleCanary) {
		return "leak"
	}
	if !strings.Contains(result.Output, "keep-sentinel") {
		return "missing-allowed-text"
	}
	return ""
}

func TestPrivacyHTTPProtectedObserver(t *testing.T) {
	t.Run("SyntheticProtectedObserver", func(t *testing.T) {
		backend := &privacyOracleBackend{testBackend: &testBackend{}, lab: privacy.NewLab(t.TempDir())}
		mux := http.NewServeMux()
		Mount(mux, backend)
		handler := Secure(mux)
		path := "/api/ui/privacy/preview"
		in := map[string]any{"mode": "text", "operation": "mask", "input": uiOracleInput, "rules": map[string]any{}, "enabled": true}
		if !strings.Contains(uiOracleInput, uiOracleCanary) {
			t.Fatal("synthetic raw source positive control did not contain canary")
		}
		if got := uiOracleProtected(0, nil); got != "not-executed" {
			t.Fatalf("empty control: %s", got)
		}
		w := uiOracleCall(t, handler, http.MethodPost, path, in)
		if reason := uiOracleProtected(w.Code, w.Body.Bytes()); reason != "" {
			t.Fatalf("protected HTTP preview: %s", reason)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("sensitive HTTP response cacheable")
		}
		var result privacy.PreviewResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { backend.lab.Clear(result.ID) })
		for _, tc := range []struct {
			name, output, reason string
		}{
			{"raw", uiOracleInput, "leak"},
			{"empty", "", "incomplete-preview"},
			{"lost-text", "<secret:synthetic>", "missing-allowed-text"},
		} {
			broken := result
			broken.Output = tc.output
			data, err := json.Marshal(broken)
			if err != nil {
				t.Fatal(err)
			}
			if got := uiOracleProtected(http.StatusOK, data); got != tc.reason {
				t.Fatalf("%s local mutation: got %s, want %s", tc.name, got, tc.reason)
			}
		}
		// JSON decoding must detect a supported escaped spelling as well.
		broken := result
		broken.Output = uiOracleInput
		data, err := json.Marshal(broken)
		if err != nil {
			t.Fatal(err)
		}
		escaped := strings.Replace(string(data), uiOracleCanary, string([]byte{92, 117, 48, 48, 53, 50, 92, 117, 48, 48, 53, 50})+"-LAB-SYN-7319", 1)
		if strings.Contains(escaped, uiOracleCanary) || uiOracleProtected(http.StatusOK, []byte(escaped)) != "leak" {
			t.Fatal("escaped canary mutation went undetected")
		}

		state := uiOracleCall(t, handler, http.MethodGet, "/api/ui/privacy", nil)
		if state.Code != http.StatusOK || strings.Contains(state.Body.String(), uiOracleCanary) || strings.Contains(state.Body.String(), result.ID) {
			t.Fatal("aggregate UI state leaked preview contents or capability")
		}
		var counts privacy.LabState
		if err := json.Unmarshal(state.Body.Bytes(), &counts); err != nil || counts.Checks < 1 || counts.Active != 1 || counts.TrafficApplied {
			t.Fatal("synthetic lab state did not record preview or falsely claimed live traffic", err)
		}
		requests := uiOracleCall(t, handler, http.MethodGet, "/api/ui/requests", nil)
		if requests.Code != http.StatusOK || strings.Contains(requests.Body.String(), uiOracleCanary) || strings.Contains(requests.Body.String(), result.ID) {
			t.Fatal("synthetic backend request list leaked canary or capability")
		}
		// This checks the synthetic backend response only, not production history.
		restored := uiOracleCall(t, handler, http.MethodPost, "/api/ui/privacy/restore", map[string]any{"id": result.ID, "input": result.Output})
		var back privacy.PreviewResult
		if restored.Code != http.StatusOK || json.Unmarshal(restored.Body.Bytes(), &back) != nil || back.Output != uiOracleInput {
			t.Fatal("authorized HTTP restore differed from source")
		}
		cleared := uiOracleCall(t, handler, http.MethodPost, "/api/ui/privacy/clear", map[string]any{"id": result.ID})
		if cleared.Code != http.StatusOK {
			t.Fatal("clear handler did not execute")
		}
		rejected := uiOracleCall(t, handler, http.MethodPost, "/api/ui/privacy/restore", map[string]any{"id": result.ID, "input": result.Output})
		if rejected.Code != http.StatusGone || strings.Contains(rejected.Body.String(), uiOracleCanary) || strings.Contains(rejected.Body.String(), result.ID) {
			t.Fatal("cleared HTTP capability remained valid or echoed sensitive input")
		}
	})
}
