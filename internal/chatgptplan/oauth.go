package chatgptplan

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sync"
)

const scopes = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"

var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,256}$`)

func validClientID(id string) bool {
	return id != "dynamic_agent_client" && clientIDPattern.MatchString(id)
}
func validIssuer(raw string) bool {
	if raw == Issuer {
		return true
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "http" && net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback() && u.Port() != "" && u.Path == "" && u.RawQuery == "" && u.User == nil && u.Fragment == ""
}

type Attempt struct {
	URL                                                 string
	verifier, nonce, state, issuer, redirectURI, hostID string
	previous                                            *Registration
	mu                                                  sync.Mutex
	callbackDone, exchanged                             bool
	code, clientID                                      string
}

func randomValue() (string, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b), err
}
func NewAttempt(issuer, redirectURI, hostID string, previous *Registration, idTokenHint string) (*Attempt, error) {
	u, err := url.Parse(redirectURI)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "/auth/callback" || u.RawQuery != "" || u.User != nil || u.Fragment != "" || !validIssuer(issuer) || !validHostID(hostID) {
		return nil, errors.New("invalid ChatGPT authorization configuration")
	}
	a := &Attempt{issuer: issuer, redirectURI: redirectURI, hostID: hostID}
	for _, p := range []*string{&a.verifier, &a.nonce, &a.state} {
		*p, err = randomValue()
		if err != nil {
			return nil, err
		}
	}
	digest := sha256.Sum256([]byte(a.verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {"dynamic_agent_client"}, "redirect_uri": {redirectURI}, "scope": {scopes}, "resource": {Resource}, "state": {a.state}, "nonce": {a.nonce}, "code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])}, "code_challenge_method": {"S256"}, "ext_agent_host_id": {hostID}, "agent_name_hint": {"Claude Router"}}
	if previous != nil {
		if previous.Issuer != issuer || previous.Subject == "" || !validClientID(previous.ClientID) || previous.HostID != hostID {
			return nil, errors.New("invalid returning ChatGPT registration")
		}
		copy := *previous
		copy.Scopes = append([]string(nil), previous.Scopes...)
		a.previous = &copy
		q.Set("client_id", previous.ClientID)
		q.Del("agent_name_hint")
		if idTokenHint != "" {
			q.Set("id_token_hint", idTokenHint)
		}
		if !previous.SharingEnabled() {
			q.Set("prompt", "consent")
		}
	}
	a.URL = issuer + "/api/accounts/authorize?" + q.Encode()
	return a, nil
}
func (a *Attempt) Callback(q url.Values) (string, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.callbackDone {
		return "", "", errors.New("authorization callback already consumed")
	}
	a.callbackDone = true
	for _, key := range []string{"state", "code", "client_id", "error"} {
		if len(q[key]) > 1 {
			return "", "", errors.New("duplicate authorization parameter")
		}
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(a.state)) != 1 {
		return "", "", errors.New("invalid authorization state")
	}
	if q.Get("error") != "" {
		return "", "", errors.New("authorization denied")
	}
	clientID := q.Get("client_id")
	if a.previous != nil {
		if clientID != "" && clientID != a.previous.ClientID {
			return "", "", errors.New("authorization client changed")
		}
		clientID = a.previous.ClientID
	}
	if !validClientID(clientID) || q.Get("code") == "" || len(q.Get("code")) > 8192 {
		return "", "", errors.New("invalid authorization callback")
	}
	a.code, a.clientID = q.Get("code"), clientID
	return a.code, clientID, nil
}
func (a *Attempt) Exchange(ctx context.Context, client *http.Client, code, clientID string) (Registration, Tokens, error) {
	a.mu.Lock()
	if a.exchanged || a.code == "" || code != a.code || clientID != a.clientID {
		a.mu.Unlock()
		return Registration{}, Tokens{}, errors.New("invalid or reused authorization exchange")
	}
	a.exchanged = true
	a.mu.Unlock()
	token, err := exchange(ctx, client, a.issuer, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {a.redirectURI}, "code_verifier": {a.verifier}, "resource": {Resource}})
	if err != nil {
		return Registration{}, Tokens{}, err
	}
	if token.ID == "" || token.Refresh == "" || token.Scope == nil {
		return Registration{}, Tokens{}, errors.New("incomplete ChatGPT registration response")
	}
	reg := Registration{Issuer: a.issuer, ClientID: clientID, HostID: a.hostID}
	if err = verifyIdentity(ctx, client, &reg, token.ID, a.nonce); err != nil {
		return Registration{}, Tokens{}, err
	}
	if a.previous != nil && reg.IdentityKey() != a.previous.IdentityKey() {
		return Registration{}, Tokens{}, errors.New("ChatGPT identity changed during authorization")
	}
	if err = applyMetadata(&reg, token); err != nil {
		return Registration{}, Tokens{}, err
	}
	return reg, Tokens{token.Access, token.Refresh, token.ID}, nil
}
func Renew(ctx context.Context, client *http.Client, reg Registration, tokens Tokens) (Registration, Tokens, error) {
	if !validIssuer(reg.Issuer) || !validClientID(reg.ClientID) || reg.Subject == "" || tokens.RefreshToken == "" {
		return Registration{}, Tokens{}, errors.New("invalid ChatGPT renewal registration")
	}
	if reg.EarliestRefreshAt.After(now()) {
		return Registration{}, Tokens{}, errors.New("ChatGPT renewal is not available yet")
	}
	token, err := exchange(ctx, client, reg.Issuer, url.Values{"grant_type": {"refresh_token"}, "client_id": {reg.ClientID}, "refresh_token": {tokens.RefreshToken}, "resource": {Resource}})
	if err != nil {
		return Registration{}, Tokens{}, err
	}
	updated := reg
	if token.ID != "" {
		if err = verifyIdentity(ctx, client, &updated, token.ID, ""); err != nil {
			return Registration{}, Tokens{}, err
		}
		if updated.IdentityKey() != reg.IdentityKey() {
			return Registration{}, Tokens{}, errors.New("ChatGPT renewal changed identity")
		}
		tokens.IDToken = token.ID
	}
	if err = applyMetadata(&updated, token); err != nil {
		return Registration{}, Tokens{}, err
	}
	tokens.AccessToken = token.Access
	if token.Refresh != "" {
		tokens.RefreshToken = token.Refresh
	}
	return updated, tokens, nil
}
