package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

// The read-only WHAM contract follows Cozyphi internal/provider/quota.go.
// Pointers preserve the distinction between unavailable and observed zero.
type codexUsageWindow struct {
	Used    *float64 `json:"used_percent"`
	Seconds int64    `json:"limit_window_seconds"`
	Reset   *int64   `json:"reset_at"`
}
type codexUsageWindows struct {
	Allowed   *bool             `json:"allowed"`
	Reached   *bool             `json:"limit_reached"`
	Primary   *codexUsageWindow `json:"primary_window"`
	Secondary *codexUsageWindow `json:"secondary_window"`
}
type codexUsagePayload struct {
	Plan       string             `json:"plan_type"`
	Limits     *codexUsageWindows `json:"rate_limit"`
	Additional []struct {
		Name    string             `json:"limit_name"`
		Feature string             `json:"metered_feature"`
		Limits  *codexUsageWindows `json:"rate_limit"`
	} `json:"additional_rate_limits"`
	Resets json.RawMessage `json:"rate_limit_reset_credits"`
}
type codexAccount struct{ ID, Email, Name, Plan string }

func (a codexAccount) key() string { return a.ID + "\x00" + a.Email }

type codexUsageRow struct {
	Name      string
	ID        string // stable name for /api/limits: [limit_name-]primary|secondary
	Seconds   int64  // window length, 0 when not reported
	Known     bool
	Remaining float64
	Used      float64
	Reset     time.Time
	ResetIn   string
	Blocked   bool
}
type codexUsageView struct {
	Provider           string
	Connected          bool
	Account            codexAccount
	Plan               string
	Limits             []codexUsageRow
	ResetsKnown        bool
	Resets             int64
	Updated, Attempted time.Time
	Error              string
}
type codexUsageCache struct {
	mu     sync.Mutex
	key    string
	view   codexUsageView
	client *http.Client
}

func accountFromCredential(c codexCredential) codexAccount {
	account := codexAccount{ID: c.Tokens.AccountID}
	for _, token := range []string{c.Tokens.IDToken, c.Tokens.AccessToken} {
		parts := strings.Split(token, ".")
		if len(parts) != 3 || len(parts[1]) > 1<<20 {
			continue
		}
		data, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			continue
		}
		var claims struct {
			Email   string `json:"email"`
			Name    string `json:"name"`
			Profile struct {
				Email string `json:"email"`
				Name  string `json:"name"`
			} `json:"https://api.openai.com/profile"`
			Auth struct {
				Plan string `json:"chatgpt_plan_type"`
			} `json:"https://api.openai.com/auth"`
		}
		if json.Unmarshal(data, &claims) != nil {
			continue
		}
		if account.Email == "" {
			account.Email = claims.Email
			if account.Email == "" {
				account.Email = claims.Profile.Email
			}
		}
		if account.Name == "" {
			account.Name = claims.Name
			if account.Name == "" {
				account.Name = claims.Profile.Name
			}
		}
		if account.Plan == "" {
			account.Plan = claims.Auth.Plan
		}
	}
	return account
}

// Reading identity does not refresh tokens or send network requests.
func (s *codexAuthStore) account() (codexAccount, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.credential
	if !s.loaded {
		var err error
		c, err = readCodexCredential(s.path)
		if err != nil {
			return codexAccount{}, false
		}
	}
	return accountFromCredential(c), true
}

func authorizeCodexUsage(req *http.Request, c codexCredential) error {
	if req == nil || req.URL == nil || req.Method != http.MethodGet || req.URL.String() != codexUsageURL {
		return errors.New("недопустимый адрес запроса лимитов Codex")
	}
	req.Header.Set("Authorization", "Bearer "+c.Tokens.AccessToken)
	req.Header.Set("ChatGPT-Account-Id", c.Tokens.AccountID)
	if residency := codexResidency(c.Tokens.AccessToken); residency != "" {
		req.Header.Set("x-openai-internal-codex-residency", residency)
	}
	req.Header.Set("originator", "claude-router")
	req.Header.Set("Accept", "application/json")
	return nil
}

func fetchCodexUsage(ctx context.Context, client *http.Client, c codexCredential) (codexUsagePayload, error) {
	var payload codexUsagePayload
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return payload, errors.New("не удалось создать запрос лимитов")
	}
	if err = authorizeCodexUsage(req, c); err != nil {
		return payload, err
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	bounded := *client
	bounded.Timeout = 10 * time.Second
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := bounded.Do(req)
	if err != nil {
		return payload, errors.New("не удалось загрузить лимиты Codex: проверьте соединение")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		if response.StatusCode == 401 || response.StatusCode == 403 {
			return payload, fmt.Errorf("Codex вернул HTTP %d: попробуйте войти заново", response.StatusCode)
		}
		return payload, fmt.Errorf("сервис лимитов Codex вернул HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return payload, errors.New("не удалось прочитать ответ сервиса лимитов Codex")
	}
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 || data[0] != '{' || json.Unmarshal(data, &payload) != nil {
		return payload, errors.New("сервис лимитов Codex вернул некорректные данные")
	}
	return payload, nil
}

func usageWindowName(seconds int64, fallback string) string {
	if seconds <= 0 {
		return fallback
	}
	for _, unit := range []struct {
		seconds int64
		name    string
	}{{604800, "нед."}, {86400, "дн."}, {3600, "ч."}, {60, "мин."}, {1, "сек."}} {
		if seconds%unit.seconds == 0 {
			return fmt.Sprintf("%d %s", seconds/unit.seconds, unit.name)
		}
	}
	return fallback
}

func decodeCodexUsage(payload codexUsagePayload, account codexAccount, now time.Time) codexUsageView {
	v := codexUsageView{Connected: true, Account: account, Plan: payload.Plan, Updated: now, Attempted: now}
	if v.Plan == "" {
		v.Plan = account.Plan
	}
	appendWindows := func(name string, windows *codexUsageWindows) {
		if windows == nil {
			return
		}
		blocked := (windows.Allowed != nil && !*windows.Allowed) || (windows.Reached != nil && *windows.Reached)
		count := 0
		for i, window := range []*codexUsageWindow{windows.Primary, windows.Secondary} {
			if window == nil {
				continue
			}
			fallback, id := "Основное окно", "primary"
			if i == 1 {
				fallback, id = "Дополнительное окно", "secondary"
			}
			label := usageWindowName(window.Seconds, fallback)
			if name != "" {
				label, id = name+" · "+label, name+"-"+id
			}
			row := codexUsageRow{Name: label, ID: id, Seconds: max(window.Seconds, 0), Blocked: blocked}
			if window.Used != nil {
				row.Known = true
				row.Used = max(0, min(100, *window.Used))
				row.Remaining = 100 - row.Used
			}
			if window.Reset != nil && *window.Reset > 0 {
				row.Reset = time.Unix(*window.Reset, 0)
			}
			v.Limits = append(v.Limits, row)
			count++
		}
		if count == 0 && blocked {
			id := name
			if name == "" {
				name, id = "Codex", "rate_limit"
			}
			v.Limits = append(v.Limits, codexUsageRow{Name: name, ID: id, Blocked: true})
		}
	}
	appendWindows("", payload.Limits)
	for _, extra := range payload.Additional {
		name := extra.Name
		if name == "" {
			name = extra.Feature
		}
		if name == "" {
			name = "Дополнительный лимит"
		}
		appendWindows(name, extra.Limits)
	}
	var reset struct {
		Available *int64 `json:"available_count"`
	}
	if json.Unmarshal(payload.Resets, &reset) == nil && reset.Available != nil && *reset.Available >= 0 {
		v.ResetsKnown = true
		v.Resets = *reset.Available
	}
	return v
}

func (cache *codexUsageCache) get(ctx context.Context, auth *codexAuthStore, force bool) codexUsageView {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	account, connected := auth.account()
	if !connected {
		cache.key = ""
		cache.view = codexUsageView{}
		return cache.view
	}
	if account.key() != cache.key {
		cache.key = account.key()
		cache.view = codexUsageView{Connected: true, Account: account, Plan: account.Plan}
	}
	if force {
		credential, err := auth.credentialFor(ctx)
		if err == nil {
			account = accountFromCredential(credential)
			if account.key() != cache.key {
				cache.key = account.key()
				cache.view = codexUsageView{Connected: true, Account: account, Plan: account.Plan}
			}
			var payload codexUsagePayload
			payload, err = fetchCodexUsage(ctx, cache.client, credential)
			if err == nil {
				cache.view = decodeCodexUsage(payload, account, time.Now())
			}
		} else {
			err = errors.New("не удалось обновить вход Codex; войдите заново через дашборд")
		}
		current, ok := auth.account()
		if !ok || current.key() != account.key() {
			cache.key = ""
			cache.view = codexUsageView{Connected: ok, Account: current, Error: "Аккаунт изменился во время загрузки. Обновите лимиты."}
			return cache.view
		}
		cache.view.Attempted = time.Now()
		if err != nil {
			cache.view.Error = err.Error()
		}
	}
	v := cache.view
	v.Limits = append([]codexUsageRow(nil), v.Limits...)
	for i := range v.Limits {
		if v.Limits[i].Reset.IsZero() {
			continue
		}
		left := time.Until(v.Limits[i].Reset)
		if left <= 0 {
			v.Limits[i].ResetIn = "ожидается обновление лимита"
		} else {
			v.Limits[i].ResetIn = "через " + quotaTimeLeft(left)
		}
	}
	return v
}

const (
	// codexUsageRefresh is how often the router asks Codex for usage on its
	// own. codexUsageMaxAge is how long /api/limits shows the answer, so
	// Codex windows survive two failed refreshes in a row.
	codexUsageRefresh = 10 * time.Minute
	codexUsageMaxAge  = 30 * time.Minute
)

// codexUsageTarget is one Codex connection as the background refresh and
// /api/limits see it: its name, the cache its settings block reads and its
// credential store.
type codexUsageTarget struct {
	provider string
	cache    *codexUsageCache
	auth     *codexAuthStore
}

// codexUsageTargets lists the Codex connections of the current settings in
// providers order. A connection without a usable store is left out; the
// settings page shows its error.
func (u *uiServer) codexUsageTargets() []codexUsageTarget {
	var targets []codexUsageTarget
	for _, p := range u.cs.get().local.Providers {
		if p.Type != "codex" {
			continue
		}
		if auth, err := codexStoreFor(p); err == nil {
			targets = append(targets, codexUsageTarget{p.Name, u.usageCache(p), auth})
		}
	}
	return targets
}

// refreshCodexUsageEvery sends the refresh button's request for every Codex
// connection, one after another, at start and on every tick until ctx ends.
// The list is read anew each time, so added and removed connections follow
// the settings. A failed request waits for the next tick, and a ticker drops
// ticks that arrive during a refresh, so a slow Codex never queues requests.
func refreshCodexUsageEvery(ctx context.Context, targets func() []codexUsageTarget, ticks <-chan time.Time) {
	refresh := func() {
		for _, t := range targets() {
			rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			if v := t.cache.get(rctx, t.auth, true); v.Connected && v.Error != "" {
				log.Printf("codex usage refresh %s: %s", t.provider, v.Error)
			}
			cancel()
		}
	}
	refresh()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			refresh()
		}
	}
}

func (u *uiServer) startCodexUsageUpdates(ctx context.Context) {
	ticker := time.NewTicker(codexUsageRefresh)
	go func() {
		defer ticker.Stop()
		refreshCodexUsageEvery(ctx, u.codexUsageTargets, activeTicks(ctx, u.life, ticker.C))
	}()
}

// activeTicks passes ticks on only while life lets this router write shared
// state, so a slot that was quiesced or drained stops asking Codex once the
// new one has taken over. Like a ticker, it drops a tick nobody is waiting for.
func activeTicks(ctx context.Context, life *lifecycle, in <-chan time.Time) <-chan time.Time {
	out := make(chan time.Time, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-in:
				if life != nil && !life.writesSharedState() {
					continue
				}
				select {
				case out <- t:
				default:
				}
			}
		}
	}()
	return out
}

func quotaTimeLeft(d time.Duration) string {
	if d < time.Minute {
		return "менее минуты"
	}
	minutes := int(d.Minutes())
	if minutes < 60 {
		return fmt.Sprintf("%d мин.", minutes)
	}
	hours := minutes / 60
	if hours < 24 {
		return fmt.Sprintf("%d ч. %d мин.", hours, minutes%60)
	}
	return fmt.Sprintf("%d дн. %d ч.", hours/24, hours%24)
}

func (u *uiServer) settingsCodexUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && !sameOriginPost(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	p, store, err := u.codexProvider(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	view := u.usageCache(p).get(ctx, store, r.Method == http.MethodPost)
	view.Provider = p.Name
	w.Header().Set("Cache-Control", "no-store")
	u.render(w, "codex-usage", view)
}

// usageCache keeps one connection's usage apart from every other's; a
// recreated name gets a fresh cache through its new id.
func (u *uiServer) usageCache(p provider) *codexUsageCache {
	u.usageMu.Lock()
	defer u.usageMu.Unlock()
	if u.codexUsages == nil {
		u.codexUsages = map[string]*codexUsageCache{}
	}
	key := p.Name + "\x00" + p.AuthID
	c, ok := u.codexUsages[key]
	if !ok {
		c = &codexUsageCache{client: u.codexUsage.client}
		u.codexUsages[key] = c
	}
	return c
}
