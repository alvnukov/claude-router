package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCodexRefreshesRevokedUnexpiredToken(t *testing.T) {
	oldAuth, oldTransport := codexAuth, http.DefaultTransport
	defer func() { codexAuth = oldAuth; http.DefaultTransport = oldTransport }()
	credential := usageCredential("same-account")
	fresh := testJWT(time.Now().Add(2 * time.Hour))
	var refreshes, attempts atomic.Int32
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"rotated-refresh"}`, fresh)
	}))
	defer oauth.Close()
	codexAuth = &codexAuthStore{loaded: true, credential: credential, path: filepath.Join(t.TempDir(), "auth.json"), issuer: oauth.URL, client: oauth.Client()}
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		attempts.Add(1)
		if r.Header.Get("Authorization") == "Bearer "+credential.Tokens.AccessToken {
			return usageResponse(401, `{"error":{"code":"token_revoked"}}`), nil
		}
		if r.Header.Get("Authorization") != "Bearer "+fresh {
			t.Error("unexpected token")
		}
		return usageResponse(200, `{"ok":true}`), nil
	})
	cand := candidate{Key: "codex/gpt-test", Provider: provider{Name: "codex", Type: "codex"}}
	res := tryModel(httptest.NewRequest("POST", "/", nil), config{firstByte: time.Second}, cand, []byte(`{}`), false)
	if res.err != nil {
		t.Fatalf("revoked unexpired token was not recovered: %v", res.err)
	}
	res.resp.Body.Close()
	res.cancel()
	if refreshes.Load() != 1 || attempts.Load() != 2 {
		t.Fatalf("refreshes=%d attempts=%d", refreshes.Load(), attempts.Load())
	}
	saved, err := readCodexCredential(codexAuth.path)
	if err != nil || saved.Tokens.AccessToken != fresh {
		t.Fatal("refresh not persisted")
	}
}

func TestCodexFailedRefreshDoesNotStormOrExposeSecrets(t *testing.T) {
	oldAuth, oldTransport := codexAuth, http.DefaultTransport
	defer func() { codexAuth = oldAuth; http.DefaultTransport = oldTransport }()
	var refreshes atomic.Int32
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { refreshes.Add(1); http.Error(w, "private-secret", 401) }))
	defer oauth.Close()
	codexAuth = &codexAuthStore{loaded: true, credential: usageCredential("account"), path: filepath.Join(t.TempDir(), "auth.json"), issuer: oauth.URL, client: oauth.Client()}
	http.DefaultTransport = usageTransport(func(*http.Request) (*http.Response, error) {
		return usageResponse(401, `{"error":{"code":"token_revoked"}}`), nil
	})
	for range 3 {
		res := tryModel(httptest.NewRequest("POST", "/", nil), config{firstByte: time.Second}, candidate{Key: "codex/gpt-test", Provider: provider{Name: "codex", Type: "codex"}}, []byte(`{}`), false)
		if res.err == nil || !strings.Contains(res.err.Error(), "войдите") || strings.Contains(res.err.Error(), "private-secret") {
			t.Fatalf("unclear or unsafe error: %v", res.err)
		}
	}
	if refreshes.Load() != 1 {
		t.Fatalf("repeated refreshes: %d", refreshes.Load())
	}
}
