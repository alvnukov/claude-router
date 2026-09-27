package ui

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type testBackend struct{ writes int }

func (*testBackend) State(context.Context) State     { return State{Lifecycle: "active"} }
func (*testBackend) Requests(url.Values) RequestList { return RequestList{Items: []Request{}} }
func (*testBackend) Detail(string) (Detail, bool)    { return Detail{}, false }
func (b *testBackend) Action(context.Context, Action) (Result, error) {
	b.writes++
	return Result{Message: "Saved"}, nil
}
func (*testBackend) Writable() bool { return true }
func testHandler(b *testBackend) http.Handler {
	mux := http.NewServeMux()
	Mount(mux, b)
	return Secure(mux)
}

func TestUIServesIndex(t *testing.T) {
	h := testHandler(&testBackend{})
	for _, path := range []string{"/", "/requests", "/routes", "/connections", "/settings"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost"+path, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "<html") {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
}
func TestUIEmbedsEveryDistFile(t *testing.T) {
	h := testHandler(&testBackend{})
	err := fs.WalkDir(assets, "dist", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		target := strings.TrimPrefix(path, "dist")
		if target == "/index.html" {
			target = "/"
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost"+target, nil))
		if w.Code != 200 {
			t.Errorf("asset %s: %d", path, w.Code)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestUIAssetsImmutableCache(t *testing.T) {
	h := testHandler(&testBackend{})
	found := false
	_ = fs.WalkDir(assets, "dist/assets", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		found = true
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost"+strings.TrimPrefix(path, "dist"), nil))
		if !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
			t.Errorf("asset lacks immutable cache: %s", path)
		}
		return nil
	})
	if !found {
		t.Fatal("no bundled assets")
	}
}
func TestUICSPNoInline(t *testing.T) {
	w := httptest.NewRecorder()
	testHandler(&testBackend{}).ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/", nil))
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") || strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Fatalf("unsafe CSP: %s", csp)
	}
}
func TestUIRejectsForeignHost(t *testing.T) {
	h := testHandler(&testBackend{})
	for _, host := range []string{"evil.test", "127.0.0.1.evil.test", "localhost.evil.test", "", "0.0.0.0", "127.0.0.2"} {
		r := httptest.NewRequest("GET", "http://localhost/api/ui/state", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Errorf("host %q: %d", host, w.Code)
		}
	}
	for _, host := range []string{"localhost", "localhost:8788", "127.0.0.1:80", "[::1]:8788", "[::1]"} {
		r := httptest.NewRequest("GET", "http://localhost/api/ui/state", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Errorf("loopback %q: %d", host, w.Code)
		}
	}
}
func TestUIRejectsCrossSitePost(t *testing.T) {
	b := &testBackend{}
	h := testHandler(b)
	for _, tc := range []struct{ origin, site string }{{"http://evil.test", ""}, {"null", ""}, {"https://localhost", ""}, {"http://localhost", "cross-site"}, {"http://localhost", "same-site"}, {"http://localhost/path", ""}} {
		r := httptest.NewRequest("POST", "http://localhost/api/ui/actions", strings.NewReader(`{"action":"save","fields":{}}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Sec-Fetch-Site", tc.site)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Errorf("%+v: %d", tc, w.Code)
		}
	}
	if b.writes != 0 {
		t.Fatal("cross-site request mutated state")
	}
}
func TestUIValidatesJSONBeforeMutation(t *testing.T) {
	b := &testBackend{}
	h := testHandler(b)
	for _, body := range []string{`{`, `{"action":"save"}`, `{"action":"save","fields":{},"typo":true}`, `{"action":"save","fields":{}} {}`, `{"action":"save","fields":{"key":"` + strings.Repeat("a", 65<<10) + `"}}`} {
		r := httptest.NewRequest("POST", "http://localhost/api/ui/actions", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Errorf("invalid JSON returned %d", w.Code)
		}
		var response map[string]string
		if json.Unmarshal(w.Body.Bytes(), &response) != nil || response["error"] == "" {
			t.Error("error is not JSON")
		}
	}
	if b.writes != 0 {
		t.Fatal("invalid JSON changed state")
	}
	r := httptest.NewRequest("POST", "http://localhost/api/ui/actions", strings.NewReader(`{"action":"save","fields":{}}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://localhost")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || b.writes != 1 {
		t.Fatalf("valid save: %d, writes %d", w.Code, b.writes)
	}
}
