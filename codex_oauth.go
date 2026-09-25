package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const codexCallbackAddr = "127.0.0.1:1455"

type codexBrowserFlow struct {
	URL         string
	state       string
	verifier    string
	redirectURI string
	issuer      string
	server      *http.Server
	listener    net.Listener
	result      chan browserResult
	once        sync.Once
}

type browserResult struct {
	code string
	err  error
}

func randomOAuthString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func startCodexBrowserFlow(ctx context.Context, addr, issuer string) (*codexBrowserFlow, error) {
	verifier, err := randomOAuthString()
	if err != nil {
		return nil, err
	}
	state, err := randomOAuthString()
	if err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp4", addr)
	if err != nil {
		return nil, fmt.Errorf("OAuth callback port %s: %w", addr, err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://localhost:%d/auth/callback", port)
	flow := &codexBrowserFlow{
		state: state, verifier: verifier, redirectURI: redirectURI, issuer: issuer,
		listener: listener, result: make(chan browserResult, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", flow.callback)
	flow.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		if err := flow.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			flow.finish(browserResult{err: err})
		}
	}()
	digest := sha256.Sum256([]byte(verifier))
	params := url.Values{
		"response_type": {"code"}, "client_id": {codexClientID}, "redirect_uri": {redirectURI},
		"scope":          {"openid profile email offline_access"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])}, "code_challenge_method": {"S256"},
		"id_token_add_organizations": {"true"}, "codex_cli_simplified_flow": {"true"},
		"state": {state}, "originator": {"claude-router"},
	}
	flow.URL = issuer + "/oauth/authorize?" + params.Encode()
	return flow, nil
}

func (f *codexBrowserFlow) finish(result browserResult) { f.once.Do(func() { f.result <- result }) }

func (f *codexBrowserFlow) callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	if q.Get("error") != "" {
		f.finish(browserResult{err: errors.New("authorization was denied")})
		writeOAuthPage(w, "Авторизация отменена", false)
		return
	}
	code := q.Get("code")
	if code == "" || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(f.state)) != 1 {
		f.finish(browserResult{err: errors.New("invalid authorization callback")})
		w.WriteHeader(http.StatusBadRequest)
		writeOAuthPage(w, "Ошибка проверки авторизации", false)
		return
	}
	f.finish(browserResult{code: code})
	writeOAuthPage(w, "Вход подтверждён. Вернитесь в панель роутера.", true)
}

func writeOAuthPage(w http.ResponseWriter, message string, ok bool) {
	color := "#c33"
	if ok {
		color = "#383"
	}
	fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>Codex</title><body style='font:16px system-ui;padding:3rem'><h1 style='color:%s'>Codex</h1><p>%s</p></body>", color, html.EscapeString(message))
}

func (s *codexAuthStore) finishBrowserFlow(ctx context.Context, flow *codexBrowserFlow) error {
	defer flow.server.Close()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	var result browserResult
	select {
	case result = <-flow.result:
	case <-ctx.Done():
		return errors.New("время входа истекло")
	}
	if result.err != nil {
		return result.err
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {result.code},
		"redirect_uri": {flow.redirectURI}, "client_id": {codexClientID}, "code_verifier": {flow.verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, flow.issuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("Codex token exchange: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Codex token exchange: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<16)+1))
	if err != nil || len(data) > 1<<16 {
		return errors.New("Codex token exchange: invalid response size")
	}
	var token struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		ID      string `json:"id_token"`
	}
	if json.Unmarshal(data, &token) != nil || token.Access == "" || token.Refresh == "" {
		return errors.New("Codex token exchange: incomplete response")
	}
	account := codexAccountID(token.ID)
	if account == "" {
		account = codexAccountID(token.Access)
	}
	if account == "" {
		return errors.New("Codex token exchange: account id missing")
	}
	var c codexCredential
	c.AuthMode = "chatgpt"
	c.Tokens.AccessToken, c.Tokens.RefreshToken, c.Tokens.IDToken, c.Tokens.AccountID = token.Access, token.Refresh, token.ID, account
	s.mu.Lock()
	defer s.mu.Unlock()
	return withFileLock(context.Background(), s.path+".lock", func() error {
		if err := s.save(c); err != nil {
			return err
		}
		s.credential, s.loaded = c, true
		s.rejectedToken, s.authProblem = "", ""
		return nil
	})
}
