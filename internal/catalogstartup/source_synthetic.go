//go:build catalogsynthetic

package catalogstartup

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"localrouter/internal/chatgptplan"
	"localrouter/internal/codextesttransport"
)

const syntheticManifestEnv = "ROUTER_CATALOG_SYNTHETIC_MANIFEST"
const maxManifestBytes = 64 << 10
const pinnedCodexModelsURL = "https://chatgpt.com/backend-api/codex/models?client_version=0.156.0"

type syntheticManifest struct {
	FixtureRoot          string   `json:"fixture_root"`
	OfficialURL          string   `json:"official_url"`
	CodexModelsURL       string   `json:"codex_models_url"`
	ChatGPTPlanModelsURL string   `json:"chatgpt_plan_models_url,omitempty"`
	ProviderOrigins      []string `json:"provider_origins"`
}

type syntheticTransport struct {
	base            *http.Transport
	official        string
	codex           *url.URL
	plan            *url.URL
	codexToken      string
	providerOrigins map[string]bool
	mu              sync.RWMutex
	models          map[string]bool
}

// ForProcess is a separate test binary source. No production build reads the
// manifest, and a synthetic build without one cannot fall back to the internet.
func ForProcess(_, _ string) (Dependencies, error) {
	if codextesttransport.DisableBackground() {
		return Dependencies{}, errors.New("incompatible synthetic build tags: catalogsynthetic and router_codex_loopback")
	}
	manifest, err := loadSyntheticManifest(os.Getenv(syntheticManifestEnv))
	if err != nil {
		return Dependencies{}, err
	}
	if _, err := loopbackURL(manifest.OfficialURL); err != nil {
		return Dependencies{}, fmt.Errorf("synthetic official URL: %w", err)
	}
	codex, err := loopbackURL(manifest.CodexModelsURL)
	if err != nil || !strings.HasSuffix(codex.Path, "/models") || codex.RawQuery != "client_version=0.156.0" {
		return Dependencies{}, errors.New("synthetic Codex URL must end in /models?client_version=0.156.0 on numeric loopback")
	}
	codexToken, err := syntheticCodexToken(manifest.FixtureRoot)
	if err != nil {
		return Dependencies{}, err
	}
	var plan *url.URL
	if manifest.ChatGPTPlanModelsURL != "" {
		plan, err = loopbackURL(manifest.ChatGPTPlanModelsURL)
		if err != nil || plan.EscapedPath() != "/v1/models" || plan.RawQuery != "" || plan.ForceQuery {
			return Dependencies{}, errors.New("synthetic ChatGPT plan URL must be /v1/models on numeric loopback")
		}
	}
	providers := make(map[string]bool, len(manifest.ProviderOrigins))
	allowedHosts := make(map[string]bool, len(manifest.ProviderOrigins)+2)
	for _, raw := range []string{manifest.OfficialURL, manifest.CodexModelsURL} {
		u, _ := url.Parse(raw) // Already checked by loopbackURL above.
		allowedHosts[u.Host] = true
	}
	if plan != nil {
		allowedHosts[plan.Host] = true
	}
	for _, raw := range manifest.ProviderOrigins {
		u, err := loopbackURL(raw + "/")
		if err != nil || raw != u.Scheme+"://"+u.Host {
			return Dependencies{}, errors.New("synthetic provider origin must be numeric loopback host:port")
		}
		providers[raw] = true
		allowedHosts[u.Host] = true
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || !allowedHosts[address] {
			return nil, errors.New("synthetic catalog dial denied")
		}
		return dialer.DialContext(ctx, network, address)
	}}
	return Dependencies{
		officialURL: manifest.OfficialURL,
		codexURL:    pinnedCodexModelsURL,
		transport:   transport,
		guard:       &syntheticTransport{base: transport, official: manifest.OfficialURL, codex: codex, plan: plan, codexToken: codexToken, providerOrigins: providers, models: make(map[string]bool)},
	}, nil
}

// Only a deliberately marked, unexpired fixture credential may be sent to
// the loopback Codex endpoint. The ordinary auth store still authorizes the
// original pinned HTTPS request before this transport sees it.
func syntheticCodexToken(root string) (string, error) {
	filePath := os.Getenv("ROUTER_CODEX_AUTH_FILE")
	if !filepath.IsAbs(filePath) || filePath != filepath.Clean(filePath) || filepath.Dir(filePath) != root {
		return "", errors.New("synthetic Codex auth must be a file directly inside fixture root")
	}
	info, err := os.Lstat(filePath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<10 || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("synthetic Codex auth must be a private bounded regular file")
	}
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", errors.New("synthetic Codex auth changed while opening")
	}
	body, err := io.ReadAll(io.LimitReader(f, 16<<10+1))
	if err != nil || len(body) > 16<<10 {
		return "", errors.New("synthetic Codex auth exceeds size limit")
	}
	var auth struct {
		AuthMode string                    `json:"auth_mode"`
		Plan     *chatgptplan.Registration `json:"plan"`
		Tokens   struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(body, &auth) != nil {
		return "", errors.New("synthetic Codex auth is not a fixture credential")
	}
	if auth.AuthMode == chatgptplan.AuthMode {
		if auth.Plan == nil || !auth.Plan.Valid() || !auth.Plan.SharingEnabled() || auth.Plan.Issuer != chatgptplan.Issuer || auth.Plan.Subject != "catalogsynthetic-account" || auth.Plan.ClientID != "catalogsynthetic-client" || !auth.Plan.ExpiresAt.After(time.Now().Add(30*time.Second)) || auth.Tokens.AccessToken != "catalogsynthetic-plan-access" || auth.Tokens.RefreshToken != "catalogsynthetic-refresh" {
			return "", errors.New("synthetic ChatGPT auth is not a fixture credential")
		}
		return auth.Tokens.AccessToken, nil
	}
	if auth.AuthMode != "chatgpt" || auth.Tokens.RefreshToken != "catalogsynthetic-refresh" || auth.Tokens.AccountID != "catalogsynthetic-account" {
		return "", errors.New("synthetic Codex auth is not a fixture credential")
	}
	parts := strings.Split(auth.Tokens.AccessToken, ".")
	if len(parts) != 3 || parts[0] != "catalogsynthetic" || parts[2] == "" {
		return "", errors.New("synthetic Codex auth is not a fixture credential")
	}
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(claims) > 1024 {
		return "", errors.New("synthetic Codex auth has invalid expiry")
	}
	var jwt struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(claims, &jwt) != nil || jwt.Exp <= time.Now().Add(30*time.Second).Unix() {
		return "", errors.New("synthetic Codex auth must be unexpired")
	}
	return auth.Tokens.AccessToken, nil
}

func (d Dependencies) ValidateProvider(name, kind, baseURL string, authID ...string) error {
	if d.guard == nil {
		return nil
	}
	if name == "" {
		return errors.New("synthetic provider has no name")
	}
	if kind == "codex" {
		if name != "codex" || len(authID) > 0 && authID[0] != "" {
			return errors.New("synthetic Codex requires the legacy fixture credential")
		}
		return nil // Codex uses the validated override, not the configured BaseURL.
	}
	u, err := loopbackURL(baseURL)
	if err != nil || u.RawQuery != "" {
		return errors.New("synthetic provider URL is not on an approved loopback origin")
	}
	g := d.guard.(*syntheticTransport)
	if !g.providerOrigins[u.Scheme+"://"+u.Host] {
		return errors.New("synthetic provider URL is not on an approved loopback origin")
	}
	endpoint := baseURL + "/models"
	if _, err := loopbackURL(endpoint); err != nil {
		return errors.New("synthetic provider models URL is not canonical")
	}
	g.mu.Lock()
	g.models[endpoint] = true
	g.mu.Unlock()
	return nil
}

func (g *syntheticTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil || req.Host != req.URL.Host || req.Method != http.MethodGet {
		return nil, errors.New("synthetic catalog target denied")
	}
	if req.URL.String() == chatgptplan.ModelsURL {
		if g.plan == nil || g.codexToken != "catalogsynthetic-plan-access" || len(req.Header.Values("Authorization")) != 1 || req.Header.Get("Authorization") != "Bearer "+g.codexToken || req.Header.Get("ChatGPT-Account-Id") != "" {
			return nil, errors.New("synthetic ChatGPT request lacks fixture authorization")
		}
		redirected := req.Clone(req.Context())
		endpoint := *g.plan
		redirected.URL = &endpoint
		redirected.Host = endpoint.Host
		return g.base.RoundTrip(redirected)
	}
	if req.URL.String() == pinnedCodexModelsURL {
		if req.Header.Get("Authorization") != "Bearer "+g.codexToken || req.Header.Get("ChatGPT-Account-Id") != "catalogsynthetic-account" || req.Header.Get("originator") != "claude-router" {
			return nil, errors.New("synthetic Codex request lacks fixture authorization")
		}
		redirected := req.Clone(req.Context())
		endpoint := *g.codex
		redirected.URL = &endpoint
		redirected.Host = endpoint.Host
		return g.base.RoundTrip(redirected)
	}
	if _, err := loopbackURL(req.URL.String()); err != nil {
		return nil, errors.New("synthetic catalog target denied")
	}
	if req.URL.String() != g.official {
		g.mu.RLock()
		allowed := g.models[req.URL.String()]
		g.mu.RUnlock()
		if !allowed {
			return nil, errors.New("synthetic catalog target not approved")
		}
	}
	return g.base.RoundTrip(req)
}

func loopbackURL(raw string) (*url.URL, error) {
	if len(raw) > 2048 || strings.Contains(raw, "%") {
		return nil, errors.New("noncanonical synthetic URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Opaque != "" || u.Fragment != "" || u.RawQuery == "" && u.ForceQuery || u.RawPath != "" || u.Path == "" || path.Clean(u.Path) != u.Path {
		return nil, errors.New("invalid synthetic URL")
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return nil, errors.New("synthetic URL needs numeric host and port")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() || net.JoinHostPort(ip.String(), port) != u.Host {
		return nil, errors.New("synthetic URL host is not canonical loopback")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return nil, errors.New("invalid synthetic URL port")
	}
	return u, nil
}

func loadSyntheticManifest(filePath string) (syntheticManifest, error) {
	var m syntheticManifest
	if !filepath.IsAbs(filePath) || filePath != filepath.Clean(filePath) {
		return m, errors.New("synthetic catalog manifest must have an absolute canonical path")
	}
	info, err := os.Lstat(filePath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxManifestBytes {
		return m, errors.New("synthetic catalog manifest must be a bounded regular file")
	}
	f, err := os.Open(filePath)
	if err != nil {
		return m, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return m, errors.New("synthetic catalog manifest changed while opening")
	}
	body, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	if err != nil || len(body) > maxManifestBytes {
		return m, errors.New("synthetic catalog manifest exceeds size limit")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("synthetic catalog manifest JSON: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return m, errors.New("synthetic catalog manifest has trailing data")
	}
	if !filepath.IsAbs(m.FixtureRoot) || m.FixtureRoot != filepath.Clean(m.FixtureRoot) {
		return m, errors.New("synthetic fixture root must be absolute and canonical")
	}
	rootInfo, err := os.Lstat(m.FixtureRoot)
	if err != nil || !rootInfo.IsDir() {
		return m, errors.New("synthetic fixture root must be a directory")
	}
	root, err := filepath.EvalSymlinks(m.FixtureRoot)
	if err != nil || root != m.FixtureRoot {
		return m, errors.New("synthetic fixture root contains a symlink")
	}
	if filepath.Dir(filePath) != root {
		return m, errors.New("synthetic catalog manifest must be directly inside fixture root")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(filePath))
	if err != nil || parent != root {
		return m, errors.New("synthetic catalog manifest must be directly inside fixture root")
	}
	if m.OfficialURL == "" || m.CodexModelsURL == "" {
		return m, errors.New("synthetic catalog endpoints required")
	}
	return m, nil
}
