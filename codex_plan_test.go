package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func planTestIssuer(t *testing.T, loginURL func() string) *httptest.Server {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var issuer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/jwks.json" {
			json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "test", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
			return
		}
		if r.URL.Path != "/api/accounts/oauth/token" {
			t.Errorf("invalid token endpoint %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		u, _ := url.Parse(loginURL())
		claims, _ := json.Marshal(map[string]any{"iss": issuer, "sub": "verified-person", "aud": "issued-router-client", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "nonce": u.Query().Get("nonce"), "email": "router@example.test"})
		unsigned := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims)
		digest := sha256.Sum256([]byte(unsigned))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		if err != nil {
			t.Error(err)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "opaque-browser", "refresh_token": "browser-refresh", "id_token": unsigned + "." + base64.RawURLEncoding.EncodeToString(sig), "token_type": "Bearer", "expires_in": 3600, "scope": "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"})
	}))
	issuer = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

func planFixture(t *testing.T, issuer string, enabled bool, expiry time.Time) codexCredential {
	t.Helper()
	scopes := []string{"openid"}
	if enabled {
		scopes = append(scopes, "chatgpt.tokens.use.direct")
	}
	raw, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt-plan", "tokens": map[string]string{"access_token": "opaque-old", "refresh_token": "old-refresh", "id_token": "hint"}, "plan": map[string]any{"issuer": issuer, "subject": "person", "client_id": "issued", "host_id": "urn:uuid:11111111-1111-4111-8111-111111111111", "scopes": scopes, "expires_at": expiry}})
	var c codexCredential
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}
func TestChatGPTPlanCredentialAndPinnedAuthorization(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			s := newCodexAuthStore()
			s.path = filepath.Join(t.TempDir(), "auth.json")
			c := planFixture(t, "https://auth.openai.com", enabled, time.Now().Add(time.Hour))
			if err := s.save(c); err != nil {
				t.Fatal(err)
			}
			if _, err := readCodexCredential(s.path); err != nil {
				t.Fatal(err)
			}
			if !s.connected() || s.signedIn() != enabled {
				t.Fatal("identity-only sign-in became routing candidate")
			}
			for _, target := range []string{"https://api.openai.com/v1/models", "https://api.openai.com/v1/responses", "https://chatgpt.com/backend-api/codex/models", "https://api.openai.com:443/v1/models", "https://api.openai.com/v1/models?evil=1", "https://evil.test/v1/models", "https://api.openai.com/v1/models/extra"} {
				method := "GET"
				if target == "https://api.openai.com/v1/responses" {
					method = "POST"
				}
				req, _ := http.NewRequest(method, target, nil)
				err := s.authorize(context.Background(), req)
				valid := enabled && (target == "https://api.openai.com/v1/models" || target == "https://api.openai.com/v1/responses")
				if (err == nil) != valid {
					t.Fatalf("target %s valid=%v err=%v", target, valid, err)
				}
				if valid && (req.Header.Get("Authorization") != "Bearer opaque-old" || req.Header.Get("ChatGPT-Account-Id") != "" || req.Header.Get("originator") != "") {
					t.Fatal("private Codex headers on public request")
				}
			}
		})
	}
}
func TestChatGPTPlanRefreshAcrossStores(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		r.ParseForm()
		if r.URL.Path != "/api/accounts/oauth/token" || r.Form.Get("client_id") != "issued" {
			t.Error("wrong token endpoint/client")
		}
		fmt.Fprint(w, `{"access_token":"rotated","refresh_token":"rotated-refresh","token_type":"Bearer","expires_in":3600}`)
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "auth.json")
	a, b := newCodexAuthStore(), newCodexAuthStore()
	a.path, b.path = path, path
	a.issuer, b.issuer = srv.URL, srv.URL
	a.client, b.client = srv.Client(), srv.Client()
	if err := a.save(planFixture(t, srv.URL, true, time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := a
			if i%2 == 1 {
				s = b
			}
			c, err := s.credentialFor(context.Background())
			if err != nil {
				t.Error(err)
			} else if c.Tokens.AccessToken != "rotated" {
				t.Error("unrotated access token")
			}
		}(i)
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Fatalf("%d refreshes", hits.Load())
	}
	disk, err := readCodexCredential(path)
	if err != nil || disk.Tokens.RefreshToken != "rotated-refresh" {
		t.Fatal("rotation not persisted")
	}
}
func TestChatGPTPlanRefreshWriteFailurePreservesMemory(t *testing.T) {
	s := newCodexAuthStore()
	s.path = filepath.Join(t.TempDir(), "auth.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := os.Remove(s.path); err != nil {
			t.Error(err)
		}
		if err := os.Mkdir(s.path, 0700); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"access_token":"unpersisted","refresh_token":"unpersisted-refresh","token_type":"Bearer","expires_in":3600}`)
	}))
	defer srv.Close()
	s.issuer = srv.URL
	s.client = srv.Client()
	s.credential = planFixture(t, srv.URL, true, time.Now().Add(-time.Hour))
	s.loaded = true
	if err := s.save(s.credential); err != nil {
		t.Fatal(err)
	}
	if _, err := s.credentialFor(context.Background()); err == nil {
		t.Fatal("failed persistence reported success")
	}
	if s.credential.Tokens.AccessToken != "opaque-old" || s.credential.Tokens.RefreshToken != "old-refresh" {
		t.Fatal("unpersisted tokens adopted")
	}
}
