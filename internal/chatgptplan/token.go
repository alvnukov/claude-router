package chatgptplan

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	oidc "github.com/coreos/go-oidc/v3/oidc"
	"io"
	"localrouter/internal/buildinfo"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func now() time.Time { return time.Now() }

type tokenResponse struct {
	Access   string          `json:"access_token"`
	Refresh  string          `json:"refresh_token"`
	ID       string          `json:"id_token"`
	Type     string          `json:"token_type"`
	Expires  int64           `json:"expires_in"`
	Scope    json.RawMessage `json:"scope"`
	Earliest json.RawMessage `json:"earliest_refresh_at"`
}

// TokenError preserves transient endpoint failures without invalidating sign-in.
type TokenError struct {
	Status    int
	Retryable bool
}

func (e *TokenError) Error() string { return fmt.Sprintf("ChatGPT token endpoint: HTTP %d", e.Status) }

func exchange(ctx context.Context, client *http.Client, issuer string, form url.Values) (tokenResponse, error) {
	var token tokenResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/api/accounts/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return token, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	resp, err := safeClient(client).Do(req)
	if err != nil {
		return token, &TokenError{Status: http.StatusServiceUnavailable, Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return token, &TokenError{Status: resp.StatusCode, Retryable: resp.StatusCode >= 500 || resp.StatusCode == 408 || resp.StatusCode == 429}
	}
	if json.NewDecoder(resp.Body).Decode(&token) != nil || token.Access == "" || !strings.EqualFold(token.Type, "Bearer") || token.Expires <= 0 || token.Expires > 7*24*3600 {
		return token, errors.New("invalid ChatGPT token response")
	}
	return token, nil
}
func applyMetadata(reg *Registration, token tokenResponse) error {
	reg.ExpiresAt = now().Add(time.Duration(token.Expires) * time.Second)
	if len(token.Scope) > 0 {
		var scope string
		if string(token.Scope) != "null" && json.Unmarshal(token.Scope, &scope) != nil {
			return errors.New("invalid ChatGPT scope response")
		}
		reg.Scopes = strings.Fields(scope)
	}
	reg.EarliestRefreshAt = time.Time{}
	if len(token.Earliest) > 0 && string(token.Earliest) != "null" {
		var unix int64
		if json.Unmarshal(token.Earliest, &unix) == nil {
			reg.EarliestRefreshAt = time.Unix(unix, 0)
		} else {
			var s string
			if json.Unmarshal(token.Earliest, &s) != nil {
				return errors.New("invalid earliest refresh time")
			}
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				reg.EarliestRefreshAt = time.Unix(n, 0)
			} else {
				parsed, err := time.Parse(time.RFC3339, s)
				if err != nil {
					return errors.New("invalid earliest refresh time")
				}
				reg.EarliestRefreshAt = parsed
			}
		}
	}
	return nil
}
func verifyIdentity(ctx context.Context, client *http.Client, reg *Registration, raw, nonce string) error {
	ctx = oidc.ClientContext(ctx, safeClient(client))
	keys := oidc.NewRemoteKeySet(ctx, reg.Issuer+"/.well-known/jwks.json")
	verifier := oidc.NewVerifier(reg.Issuer, keys, &oidc.Config{ClientID: reg.ClientID, SupportedSigningAlgs: []string{"RS256"}})
	id, err := verifier.Verify(ctx, raw)
	if err != nil {
		return errors.New("ChatGPT identity token verification failed")
	}
	var claims struct {
		Nonce string `json:"nonce"`
		Iat   int64  `json:"iat"`
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if id.Claims(&claims) != nil || id.Subject == "" || claims.Iat <= 0 || claims.Iat > now().Add(5*time.Second).Unix() || id.Expiry.Before(now().Add(-5*time.Second)) || (nonce != "" && subtle.ConstantTimeCompare([]byte(nonce), []byte(claims.Nonce)) != 1) {
		return errors.New("invalid ChatGPT identity claims")
	}
	reg.Subject, reg.Email, reg.Name = id.Subject, claims.Email, claims.Name
	return nil
}

type boundedTransport struct{ base http.RoundTripper }

func (b boundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := b.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	resp.Body.Close()
	if err != nil || len(raw) > 64<<10 {
		return nil, errors.New("ChatGPT authorization response too large or unreadable")
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp, nil
}
func safeClient(client *http.Client) *http.Client {
	c := http.Client{Timeout: 20 * time.Second}
	if client != nil {
		c = *client
	}
	if c.Timeout == 0 {
		c.Timeout = 20 * time.Second
	}
	transport := c.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	c.Transport = boundedTransport{transport}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}
