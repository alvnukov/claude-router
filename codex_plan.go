package main

import (
	"context"
	"errors"
	"fmt"
	"localrouter/internal/chatgptplan"
	"localrouter/internal/platform"
	"net"
	"net/http"
	"path/filepath"
	"time"
)

type codexAccountContextKey struct{}

func (c codexCredential) accountKey() string {
	if c.AuthMode == chatgptplan.AuthMode && c.Plan != nil {
		return c.Plan.IdentityKey()
	}
	return c.Tokens.AccountID
}
func (c codexCredential) expiresAt() time.Time {
	if c.AuthMode == chatgptplan.AuthMode && c.Plan != nil {
		return c.Plan.ExpiresAt
	}
	return jwtExpiry(c.Tokens.AccessToken)
}
func (c codexCredential) sharingEnabled() bool {
	return c.AuthMode != chatgptplan.AuthMode || (c.Plan != nil && c.Plan.SharingEnabled())
}
func applyCodexAuthorization(req *http.Request, c codexCredential) {
	req.Header.Set("Authorization", "Bearer "+c.Tokens.AccessToken)
	req.Header.Del("ChatGPT-Account-Id")
	req.Header.Del("x-openai-internal-codex-residency")
	req.Header.Del("originator")
	if c.AuthMode != chatgptplan.AuthMode {
		req.Header.Set("ChatGPT-Account-Id", c.Tokens.AccountID)
		if residency := codexResidency(c.Tokens.AccessToken); residency != "" {
			req.Header.Set("x-openai-internal-codex-residency", residency)
		}
		req.Header.Set("originator", "claude-router")
	}
}
func (s *codexAuthStore) startPlanBrowserFlow(ctx context.Context, addr string) (*codexBrowserFlow, error) {
	hostID, err := chatgptplan.HostID(ctx, filepath.Join(filepath.Dir(s.path), "chatgpt-host-id"))
	if err != nil {
		return nil, err
	}
	var previous *chatgptplan.Registration
	hint := ""
	s.mu.Lock()
	c := s.credential
	if !s.loaded {
		c, _ = readCodexCredential(s.path)
	}
	if c.AuthMode == chatgptplan.AuthMode && c.Plan != nil {
		copy := *c.Plan
		previous = &copy
		hint = c.Tokens.IDToken
	}
	s.mu.Unlock()
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp4", addr)
	if err != nil {
		return nil, fmt.Errorf("OAuth callback unavailable: %w", err)
	}
	redirect := fmt.Sprintf("http://127.0.0.1:%d/auth/callback", listener.Addr().(*net.TCPAddr).Port)
	attempt, err := chatgptplan.NewAttempt(s.issuer, redirect, hostID, previous, hint)
	if err != nil {
		listener.Close()
		return nil, err
	}
	flow := &codexBrowserFlow{URL: attempt.URL, redirectURI: redirect, issuer: s.issuer, plan: attempt, listener: listener, result: make(chan browserResult, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", flow.callback)
	flow.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		if err := flow.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			flow.finish(browserResult{err: err})
		}
	}()
	return flow, nil
}
func (s *codexAuthStore) adoptPlanLogin(ctx context.Context, flow *codexBrowserFlow, result browserResult, stillCurrent func() bool) error {
	reg, tokens, err := flow.plan.Exchange(ctx, s.client, result.code, result.clientID)
	if err != nil {
		return err
	}
	c := codexCredential{AuthMode: chatgptplan.AuthMode, Plan: &reg}
	c.Tokens.AccessToken, c.Tokens.RefreshToken, c.Tokens.IDToken = tokens.AccessToken, tokens.RefreshToken, tokens.IDToken
	s.mu.Lock()
	defer s.mu.Unlock()
	return platform.WithLock(ctx, s.path+".lock", func() error {
		if s.life != nil && !s.life.writesSharedState() {
			return errors.New("login unavailable while router is not active")
		}
		if stillCurrent != nil && !stillCurrent() {
			return errors.New("подключение изменилось во время входа; войдите заново")
		}
		if err := s.save(c); err != nil {
			return err
		}
		s.credential, s.loaded = c, true
		s.rejectedToken, s.authProblem = "", ""
		return nil
	})
}
