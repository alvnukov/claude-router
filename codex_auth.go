package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	codexBaseURL  = "https://chatgpt.com/backend-api/codex"
	codexIssuer   = "https://auth.openai.com"
	codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

// The router owns its ChatGPT OAuth credential. A separate explicit action may
// import a Codex CLI login without altering the CLI's auth store.
type codexCredential struct {
	AuthMode string `json:"auth_mode"`
	Tokens   struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
}

type codexAuthStore struct {
	rejectedToken string
	rejectedAt    time.Time
	authProblem   string
	mu            sync.Mutex
	life          *lifecycle
	credential    codexCredential
	loaded        bool
	path          string
	cliPath       string
	issuer        string
	client        *http.Client
}

var codexAuth = newCodexAuthStore()

func newCodexAuthStore() *codexAuthStore {
	home, _ := os.UserHomeDir()
	cliHome := os.Getenv("CODEX_HOME")
	if cliHome == "" {
		cliHome = filepath.Join(home, ".codex")
	}
	path := os.Getenv("ROUTER_CODEX_AUTH_FILE")
	if path == "" {
		path = filepath.Join(home, ".config", "claude-router", "codex-auth.json")
	}
	return &codexAuthStore{
		path: path, cliPath: filepath.Join(cliHome, "auth.json"), issuer: codexIssuer,
		client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

func readCodexCredential(path string) (codexCredential, error) {
	var c codexCredential
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if len(data) > 1<<20 {
		return c, errors.New("Codex credential file is too large")
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	if c.AuthMode != "chatgpt" || c.Tokens.AccessToken == "" || c.Tokens.RefreshToken == "" || c.Tokens.AccountID == "" {
		return c, errors.New("Codex CLI is not signed in with ChatGPT")
	}
	return c, nil
}

func (s *codexAuthStore) credentialFor(ctx context.Context) (codexCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded || (s.life != nil && !s.life.writesSharedState()) {
		c, err := readCodexCredential(s.path)
		if err != nil {
			return c, fmt.Errorf("Codex login: %w; войдите через панель роутера", err)
		}
		s.credential, s.loaded = c, true
		s.rejectedToken, s.authProblem = "", ""
	}
	if jwtExpiry(s.credential.Tokens.AccessToken).After(time.Now().Add(30 * time.Second)) {
		return s.credential, nil
	}
	if s.life != nil && !s.life.writesSharedState() {
		return codexCredential{}, errors.New("Codex token expired while router is quiesced; retry after deployment")
	}
	if err := s.refreshLocked(ctx); err != nil {
		return codexCredential{}, err
	}
	return s.credential, nil
}

func (s *codexAuthStore) refreshLocked(ctx context.Context) error {
	return withFileLock(ctx, s.path+".lock", func() error {
		if disk, err := readCodexCredential(s.path); err == nil {
			if disk.Tokens.RefreshToken != s.credential.Tokens.RefreshToken || disk.Tokens.AccessToken != s.credential.Tokens.AccessToken {
				s.credential = disk
				if jwtExpiry(disk.Tokens.AccessToken).After(time.Now().Add(30 * time.Second)) {
					return nil
				}
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		return s.refresh(ctx)
	})
}

func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[1]) > 1<<20 {
		return time.Time{}
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(data, &claims) != nil {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

func (s *codexAuthStore) refresh(ctx context.Context) error {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {s.credential.Tokens.RefreshToken}, "client_id": {codexClientID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.issuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("Codex token refresh: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Codex token refresh: HTTP %d; run codex login", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16+1))
	if err != nil || len(data) > 1<<16 {
		return errors.New("Codex token refresh: invalid response size")
	}
	var token struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		ID      string `json:"id_token"`
	}
	if json.Unmarshal(data, &token) != nil || token.Access == "" {
		return errors.New("Codex token refresh: invalid response")
	}
	c := s.credential
	c.Tokens.AccessToken = token.Access
	if token.Refresh != "" {
		c.Tokens.RefreshToken = token.Refresh
	}
	if token.ID != "" {
		c.Tokens.IDToken = token.ID
	}
	if account := codexAccountID(c.Tokens.IDToken); account != "" {
		c.Tokens.AccountID = account
	}
	s.credential = c
	if err := s.save(c); err != nil {
		return err
	}
	return nil
}

func codexAccountID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[1]) > 1<<20 {
		return ""
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		AccountID string `json:"chatgpt_account_id"`
		Auth      struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(data, &claims) != nil {
		return ""
	}
	if claims.AccountID != "" {
		return claims.AccountID
	}
	return claims.Auth.AccountID
}

func (s *codexAuthStore) save(c codexCredential) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".codex-auth-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := io.Copy(tmp, bytes.NewReader(data)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

func (s *codexAuthStore) importFromCLI() error {
	if s.life != nil && !s.life.writesSharedState() {
		return errors.New("Codex login unavailable while router is not active")
	}
	c, err := readCodexCredential(s.cliPath)
	if err != nil {
		return fmt.Errorf("Codex login: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return withFileLock(context.Background(), s.path+".lock", func() error {
		if s.life != nil && !s.life.writesSharedState() {
			return errors.New("Codex login unavailable while router is not active")
		}
		if err := s.save(c); err != nil {
			return err
		}
		s.credential, s.loaded = c, true
		s.rejectedToken, s.authProblem = "", ""
		return nil
	})
}

func (s *codexAuthStore) connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return true
	}
	_, err := readCodexCredential(s.path)
	return err == nil
}

// signedIn reports whether the store can authorize a request without a new
// sign-in: a credential is loaded or on disk, and it was not rejected. Pool
// selection asks this of every member, so it never waits for the lock: a
// holder is loading or refreshing the token, which can take seconds, and the
// member stays a candidate whose own request finds out.
func (s *codexAuthStore) signedIn() bool {
	if !s.mu.TryLock() {
		return true
	}
	defer s.mu.Unlock()
	if s.authProblem != "" {
		return false
	}
	if s.loaded {
		return true
	}
	_, err := readCodexCredential(s.path)
	return err == nil
}

func (s *codexAuthStore) authorize(ctx context.Context, req *http.Request) error {
	// Subscription tokens must never be sent to a configurable third-party URL.
	if req.URL.Scheme != "https" || req.URL.Host != "chatgpt.com" || !strings.HasPrefix(req.URL.EscapedPath(), "/backend-api/codex/") || req.URL.User != nil {
		return errors.New("Codex request target is not the pinned endpoint")
	}
	c, err := s.credentialFor(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Tokens.AccessToken)
	req.Header.Set("ChatGPT-Account-Id", c.Tokens.AccountID)
	if residency := codexResidency(c.Tokens.AccessToken); residency != "" {
		req.Header.Set("x-openai-internal-codex-residency", residency)
	}
	req.Header.Set("originator", "claude-router")
	return nil
}

func codexResidency(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[1]) > 1<<20 {
		return ""
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Residency string `json:"chatgpt_compute_residency"`
		Auth      struct {
			Residency string `json:"chatgpt_compute_residency"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(data, &claims) != nil {
		return ""
	}
	residency := claims.Auth.Residency
	if residency == "" {
		residency = claims.Residency
	}
	if residency == "no_constraint" {
		return ""
	}
	return residency
}

var errCodexSignIn = errors.New("сессия Codex недействительна; войдите заново через дашборд")

// A server can revoke a token before its JWT expiry. Refresh at most once for
// concurrent rejections of the same token; failed refreshes have a cooldown.
func (s *codexAuthStore) refreshRejected(ctx context.Context, rejected, account string) (codexCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded || s.credential.Tokens.AccountID != account {
		return codexCredential{}, errors.New("аккаунт Codex изменился; повторите запрос")
	}
	if s.credential.Tokens.AccessToken != rejected {
		return s.credential, nil
	}
	if s.rejectedToken == rejected && time.Since(s.rejectedAt) < time.Minute {
		return codexCredential{}, errCodexSignIn
	}
	s.rejectedToken, s.rejectedAt = rejected, time.Now()
	if s.life != nil && !s.life.writesSharedState() {
		if disk, err := readCodexCredential(s.path); err == nil && disk.Tokens.AccessToken != rejected {
			s.credential = disk
			return disk, nil
		}
		return codexCredential{}, errCodexSignIn
	}
	if err := s.refreshLocked(ctx); err != nil {
		s.authProblem = errCodexSignIn.Error()
		return codexCredential{}, errCodexSignIn
	}
	s.rejectedToken, s.authProblem = "", ""
	return s.credential, nil
}

func (s *codexAuthStore) authStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authProblem
}

// Retry only a rejected, unstarted request. Never replay a streaming response.
func (s *codexAuthStore) doWithReauth(client *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	resp.Body.Close()
	if req.GetBody == nil {
		return nil, errCodexSignIn
	}
	credential, err := s.refreshRejected(req.Context(), strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "), req.Header.Get("ChatGPT-Account-Id"))
	if err != nil {
		return nil, err
	}
	retry := req.Clone(req.Context())
	retry.Body, err = req.GetBody()
	if err != nil {
		return nil, errors.New("не удалось повторить запрос Codex")
	}
	retry.Header.Set("Authorization", "Bearer "+credential.Tokens.AccessToken)
	retry.Header.Set("ChatGPT-Account-Id", credential.Tokens.AccountID)
	retry.Header.Del("x-openai-internal-codex-residency")
	if residency := codexResidency(credential.Tokens.AccessToken); residency != "" {
		retry.Header.Set("x-openai-internal-codex-residency", residency)
	}
	resp, err = client.Do(retry)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		s.mu.Lock()
		if s.credential.Tokens.AccessToken == credential.Tokens.AccessToken {
			s.authProblem = errCodexSignIn.Error()
			s.rejectedToken, s.rejectedAt = credential.Tokens.AccessToken, time.Now()
		}
		s.mu.Unlock()
	}
	return resp, err
}
