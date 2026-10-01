package chatgptplan

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
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRegistrationAndVerification(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "signature", "issuer", "audience", "nonce", "expired", "subject", "iat", "returning-subject", "identity-only"} {
		t.Run(bad, func(t *testing.T) {
			var attempt *Attempt
			var issuer string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/.well-known/jwks.json":
					json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "test", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
				case "/api/accounts/oauth/token":
					r.ParseForm()
					if r.Form.Get("client_id") != "issued-router-client" || r.Form.Get("resource") != "https://api.openai.com/v1" || r.Form.Get("redirect_uri") != "http://127.0.0.1:1455/auth/callback" || r.Form.Get("code_verifier") != attempt.verifier {
						t.Errorf("invalid exchange binding")
					}
					q, _ := url.Parse(attempt.URL)
					claims := map[string]any{"iss": issuer, "aud": "issued-router-client", "sub": "person", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": q.Query().Get("nonce"), "email": "verified@example.test"}
					switch bad {
					case "issuer":
						claims["iss"] = "https://evil.test"
					case "audience":
						claims["aud"] = "other-client"
					case "nonce":
						claims["nonce"] = "wrong"
					case "expired":
						claims["exp"] = time.Now().Add(-time.Hour).Unix()
					case "subject":
						delete(claims, "sub")
					case "iat":
						delete(claims, "iat")
					case "returning-subject":
						claims["sub"] = "other-person"
					}
					id := signedJWT(t, key, claims)
					if bad == "signature" {
						p := strings.Split(id, ".")
						p[2] = base64.RawURLEncoding.EncodeToString(make([]byte, 256))
						id = strings.Join(p, ".")
					}
					scope := "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
					if bad == "identity-only" {
						scope = "openid profile email"
					}
					json.NewEncoder(w).Encode(map[string]any{"access_token": "opaque-access", "refresh_token": "rotating-refresh", "id_token": id, "token_type": "Bearer", "expires_in": 3600, "scope": scope})
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			issuer = srv.URL
			var previous *Registration
			if bad == "returning-subject" {
				previous = &Registration{Issuer: issuer, Subject: "person", ClientID: "issued-router-client", HostID: "urn:uuid:11111111-1111-4111-8111-111111111111"}
			}
			attempt, err = NewAttempt(issuer, "http://127.0.0.1:1455/auth/callback", "urn:uuid:11111111-1111-4111-8111-111111111111", previous, "old-id-hint")
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(attempt.URL)
			q := u.Query()
			if u.Path != "/api/accounts/authorize" || q.Get("resource") != "https://api.openai.com/v1" || q.Get("nonce") == "" || q.Get("code_challenge_method") != "S256" || q.Get("scope") != "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct" {
				t.Fatal("invalid authorize contract")
			}
			if previous == nil && (q.Get("client_id") != "dynamic_agent_client" || q.Get("agent_name_hint") != "Claude Router") {
				t.Fatal("borrowed client identity")
			}
			if previous != nil && (q.Get("client_id") != "issued-router-client" || q.Get("id_token_hint") != "old-id-hint") {
				t.Fatal("registration not reused")
			}
			code, cid, err := attempt.Callback(url.Values{"state": {q.Get("state")}, "code": {"code"}, "client_id": {"issued-router-client"}})
			if err != nil {
				t.Fatal(err)
			}
			reg, tokens, err := attempt.Exchange(context.Background(), srv.Client(), code, cid)
			wantOK := bad == "" || bad == "identity-only"
			if !wantOK {
				if err == nil {
					t.Fatal("unverified identity accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tokens.AccessToken != "opaque-access" || reg.Subject != "person" || reg.Email != "verified@example.test" || !reg.ExpiresAt.After(time.Now().Add(59*time.Minute)) || reg.SharingEnabled() != (bad != "identity-only") {
				t.Fatal("invalid verified registration")
			}
			if _, _, err = attempt.Exchange(context.Background(), srv.Client(), code, cid); err == nil {
				t.Fatal("exchange reused")
			}
		})
	}
}

func signedJWT(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(claims)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test"}`))
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestCallbackRejectsSubstitutionAndReuse(t *testing.T) {
	for _, bad := range []string{"state", "denied-state", "client", "dynamic", "duplicate", "missing"} {
		t.Run(bad, func(t *testing.T) {
			a, err := NewAttempt("https://auth.openai.com", "http://127.0.0.1:1234/auth/callback", "urn:uuid:11111111-1111-4111-8111-111111111111", &Registration{Issuer: Issuer, Subject: "person", ClientID: "issued", HostID: "urn:uuid:11111111-1111-4111-8111-111111111111"}, "hint")
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(a.URL)
			q := url.Values{"state": {u.Query().Get("state")}, "code": {"code"}, "client_id": {"issued"}}
			switch bad {
			case "state":
				q.Set("state", "wrong")
			case "denied-state":
				q.Set("state", "wrong")
				q.Set("error", "access_denied")
			case "client":
				q.Set("client_id", "other")
			case "dynamic":
				q.Set("client_id", "dynamic_agent_client")
			case "duplicate":
				q.Add("state", q.Get("state"))
			case "missing":
				q.Del("code")
			}
			if _, _, err = a.Callback(q); err == nil {
				t.Fatal("invalid callback accepted")
			}
		})
	}
}

func TestHostIdentityPersistsAcrossConcurrentCreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host-id")
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := HostID(context.Background(), path)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first || !strings.HasPrefix(id, "urn:uuid:") {
			t.Fatal("host identity changed")
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("host identity is not private")
	}
	if err = os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = HostID(context.Background(), path); err == nil {
		t.Fatal("invalid persisted identity accepted")
	}
}

func TestRenewUsesRegistrationAndRotatesMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.URL.Path != "/api/accounts/oauth/token" || r.Form.Get("client_id") != "issued" || r.Form.Get("resource") != "https://api.openai.com/v1" || r.Form.Get("refresh_token") != "old-refresh" || r.Form.Has("scope") {
			t.Error("invalid renewal")
		}
		fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"new-refresh","token_type":"Bearer","expires_in":3600,"scope":"openid"}`)
	}))
	defer srv.Close()
	reg, tok, err := Renew(context.Background(), srv.Client(), Registration{Issuer: srv.URL, Subject: "person", ClientID: "issued", Scopes: []string{"chatgpt.tokens.use.direct"}}, Tokens{AccessToken: "old-access", RefreshToken: "old-refresh", IDToken: "old-id"})
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "new-access" || tok.RefreshToken != "new-refresh" || tok.IDToken != "old-id" || reg.SharingEnabled() || !reg.ExpiresAt.After(time.Now()) {
		t.Fatal("renewal metadata not rotated together")
	}
}
