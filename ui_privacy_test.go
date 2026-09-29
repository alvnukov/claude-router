package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	conf "localrouter/internal/config"
	"localrouter/internal/privacy"
)

func TestPrivacyHTTPPreviewAndConfiguration(t *testing.T) {
	u, h := testUI(t)
	u.cs = conf.NewStore(u.cs.Get(), filepath.Join(t.TempDir(), "providers.json"))
	body := map[string]any{"mode": "text", "input": "10.3.4.5\npassword=CANARY-only-in-preview", "rules": map[string]any{}, "enabled": true}
	w := apiCall(t, h, "POST", "/api/ui/privacy/preview", body)
	if w.Code != 200 {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	var p privacy.PreviewResult
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	defer u.PrivacyLab().Clear(p.ID)
	if !p.Roundtrip || strings.Contains(p.Output, "CANARY") {
		t.Fatal("not masked")
	}
	w = apiCall(t, h, "POST", "/api/ui/privacy/restore", map[string]string{"id": p.ID, "input": p.Output})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "CANARY") {
		t.Fatal("restore failed")
	}
	for _, path := range []string{"/api/ui/privacy", "/api/ui/state", "/api/ui/requests"} {
		w = apiCall(t, h, "GET", path, nil)
		if w.Code != 200 || strings.Contains(w.Body.String(), "CANARY") || strings.Contains(w.Body.String(), p.ID) {
			t.Fatalf("unsafe aggregate %s", path)
		}
	}
	r := httptestPrivacy("POST", "/api/ui/privacy/preview", `{"mode":"text"}`)
	r.Header.Set("Origin", "https://foreign.example")
	w = callPrivacy(h, r)
	if w.Code != 403 {
		t.Fatal("foreign origin accepted")
	}
}

func httptestPrivacy(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}
func callPrivacy(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestPrivacyHTTPRejectsAmbiguityAndNeverEchoesRules(t *testing.T) {
	u, h := testUI(t)
	u.cs = conf.NewStore(u.cs.Get(), filepath.Join(t.TempDir(), "providers.json"))
	for _, draft := range []string{`{"domains":"CANARY"}`, `{"filters":{"secret":true,"secret":false}}`} {
		w := apiCall(t, h, "POST", "/api/ui/privacy/validate", map[string]string{"rules": draft})
		if w.Code != 400 || strings.Contains(w.Body.String(), "CANARY") {
			t.Fatal("raw draft accepted or reflected")
		}
	}
	for _, body := range []string{
		`{"enabled":true,"enabled":false}`,
		`{"mode":"text","input":"SENSITIVE-CANARY","rules":{"detection":{"ipv4":"CANARY("}},"enabled":true}`,
		`{"mode":"text","input":"SENSITIVE-CANARY","rules":{"filters":{"secret":null}},"enabled":true}`,
		`{"mode":"text","input":"SENSITIVE-CANARY","rules":{},"enabled":true,"path":"/tmp/CANARY"}`,
	} {
		w := callPrivacy(h, httptestPrivacy("POST", "/api/ui/privacy/preview", body))
		if w.Code != 400 || strings.Contains(w.Body.String(), "CANARY") {
			t.Fatalf("unsafe error: %d %s", w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("sensitive response could be cached")
		}
	}
	w := callPrivacy(h, httptestPrivacy("POST", "/api/ui/privacy/restore", `{"id":"missing","input":"CANARY"}`))
	if w.Code != 410 || strings.Contains(w.Body.String(), "CANARY") {
		t.Fatal("missing handle accepted or reflected")
	}
	r := httptestPrivacy("POST", "/api/ui/privacy/preview", `{}`)
	r.Header.Set("Content-Type", "text/plain")
	if w = callPrivacy(h, r); w.Code != 415 {
		t.Fatal("content type ignored")
	}
}

func TestPrivacyHTTPRuntimeStatus(t *testing.T) {
	u, h := testUI(t)
	u.cs = conf.NewStore(u.cs.Get(), filepath.Join(t.TempDir(), "providers.json"))
	w := apiCall(t, h, "GET", "/api/ui/privacy", nil)
	var result struct {
		TrafficApplied bool                 `json:"trafficApplied"`
		Traffic        privacy.RuntimeState `json:"traffic"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.TrafficApplied || result.Traffic.Status != "missing" || !result.Traffic.Buffered {
		t.Fatal("runtime status missing")
	}
	writeTrafficConfig(t, filepath.Dir(u.cs.Path()), `{}`)
	w = apiCall(t, h, "GET", "/api/ui/privacy", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Traffic.Enabled || result.Traffic.Status != "ready" {
		t.Fatal("enabled policy not reflected")
	}
}
