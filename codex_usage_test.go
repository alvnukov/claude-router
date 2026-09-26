package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"localrouter/internal/history"
)

type usageTransport func(*http.Request) (*http.Response, error)

func (f usageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func usageResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}
func usageCredential(id string) codexCredential {
	c := codexCredential{AuthMode: "chatgpt"}
	c.Tokens.AccountID = id
	c.Tokens.AccessToken = testJWT(time.Now().Add(time.Hour))
	c.Tokens.RefreshToken = "private-refresh"
	claims := `{"email":"person@example.test","name":"Test Person","https://api.openai.com/auth":{"chatgpt_plan_type":"pro"}}`
	c.Tokens.IDToken = "x." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".x"
	return c
}

func TestCodexUsageWindowsAndUnknowns(t *testing.T) {
	var payload codexUsagePayload
	fixture := `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":2000000000},"secondary_window":{"used_percent":91,"limit_window_seconds":604800}},"additional_rate_limits":[{"limit_name":"Astra","rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"limit_window_seconds":18000}}},{"metered_feature":"Luna","rate_limit":{"primary_window":{"used_percent":null}}}],"rate_limit_reset_credits":{"available_count":2}}`
	if err := json.Unmarshal([]byte(fixture), &payload); err != nil {
		t.Fatal(err)
	}
	account := accountFromCredential(usageCredential("acct-test"))
	v := decodeCodexUsage(payload, account, time.Now())
	if account.Email != "person@example.test" || account.Plan != "pro" || len(v.Limits) != 4 || !v.ResetsKnown || v.Resets != 2 {
		t.Fatal("account/windows/reset mapping")
	}
	if !v.Limits[0].Known || v.Limits[0].Remaining != 100 || v.Limits[0].Reset.Unix() != 2000000000 || v.Limits[0].Name != "5 ч." {
		t.Fatal("zero/reset window mapping")
	}
	if v.Limits[1].Remaining != 9 || !v.Limits[2].Blocked || v.Limits[3].Known {
		t.Fatal("remaining/missing window mapping")
	}
	for _, raw := range []string{`{}`, `{"rate_limit":null}`, `{"rate_limit":{"primary_window":{"used_percent":null}},"rate_limit_reset_credits":{"available_count":"private-body"}}`} {
		var p codexUsagePayload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		got := decodeCodexUsage(p, account, time.Now())
		if got.ResetsKnown {
			t.Fatal("missing reset count became zero")
		}
		for _, row := range got.Limits {
			if row.Known {
				t.Fatal("missing usage became zero")
			}
		}
	}
	var zero codexUsagePayload
	if err := json.Unmarshal([]byte(`{"rate_limit_reset_credits":{"available_count":0}}`), &zero); err != nil {
		t.Fatal(err)
	}
	if got := decodeCodexUsage(zero, account, time.Now()); !got.ResetsKnown || got.Resets != 0 {
		t.Fatal("observed zero resets lost")
	}
}

func TestCodexUsageRequestIsReadOnlyAndSanitized(t *testing.T) {
	c := usageCredential("acct-test")
	calls := 0
	client := &http.Client{Transport: usageTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" || r.URL.String() != codexUsageURL || r.Header.Get("Authorization") != "Bearer "+c.Tokens.AccessToken || r.Header.Get("ChatGPT-Account-Id") != c.Tokens.AccountID {
			t.Fatal("wrong endpoint/auth")
		}
		return usageResponse(200, " \n{} "), nil
	})}
	if _, err := fetchCodexUsage(t.Context(), client, c); err != nil || calls != 1 {
		t.Fatal("fetch failed")
	}
	for _, target := range []string{codexUsageURL + "?x=1", codexUsageURL + "#fragment", "https://chatgpt.com/backend-api/wham/%75sage", "https://other.test/backend-api/wham/usage", "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"} {
		r, _ := http.NewRequest("GET", target, nil)
		if authorizeCodexUsage(r, c) == nil || r.Header.Get("Authorization") != "" {
			t.Fatal("unsafe endpoint authorized")
		}
	}
	r, _ := http.NewRequest("POST", codexUsageURL, nil)
	if authorizeCodexUsage(r, c) == nil {
		t.Fatal("mutation authorized")
	}
	for _, tc := range []struct {
		code int
		body string
	}{{401, "private-body"}, {500, "private-body"}, {200, "private-body"}, {200, strings.Repeat("x", (1<<20)+1)}} {
		client.Transport = usageTransport(func(*http.Request) (*http.Response, error) { return usageResponse(tc.code, tc.body), nil })
		_, err := fetchCodexUsage(t.Context(), client, c)
		if err == nil || strings.Contains(err.Error(), "private-body") {
			t.Fatal("missing or unsafe error")
		}
	}
	client.Transport = usageTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("private-token-and-url") })
	if _, err := fetchCodexUsage(t.Context(), client, c); err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatal("transport details leaked")
	}
	calls = 0
	client.Transport = usageTransport(func(*http.Request) (*http.Response, error) {
		calls++
		resp := usageResponse(302, "")
		resp.Header.Set("Location", "https://other.test/leak")
		return resp, nil
	})
	if _, err := fetchCodexUsage(t.Context(), client, c); err == nil || calls != 1 {
		t.Fatal("redirect followed")
	}
}

func TestCodexUsageOnlyManualRefreshAndAccountIsolation(t *testing.T) {
	auth := &codexAuthStore{loaded: true, credential: usageCredential("account-one")}
	calls := 0
	fail := false
	switchAccount := false
	cache := codexUsageCache{client: &http.Client{Transport: usageTransport(func(*http.Request) (*http.Response, error) {
		calls++
		if switchAccount {
			auth.mu.Lock()
			auth.credential = usageCredential("account-three")
			auth.mu.Unlock()
		}
		if fail {
			return usageResponse(503, "private-body"), nil
		}
		return usageResponse(200, `{"rate_limit":{"primary_window":{"used_percent":25}},"rate_limit_reset_credits":{"available_count":3}}`), nil
	})}}
	if v := cache.get(t.Context(), auth, false); !v.Connected || !v.Updated.IsZero() || calls != 0 {
		t.Fatal("GET fetched usage")
	}
	v := cache.get(t.Context(), auth, true)
	if calls != 1 || len(v.Limits) != 1 || v.Limits[0].Remaining != 75 || v.Resets != 3 {
		t.Fatal("manual refresh failed")
	}
	cache.view.Attempted = time.Now().Add(-24 * time.Hour)
	if v = cache.get(t.Context(), auth, false); calls != 1 || len(v.Limits) != 1 {
		t.Fatal("stale GET fetched usage")
	}
	fail = true
	v = cache.get(t.Context(), auth, true)
	if v.Error == "" || v.Updated.IsZero() || v.Resets != 3 {
		t.Fatal("failure lost last good snapshot")
	}
	auth.mu.Lock()
	auth.credential = usageCredential("account-two")
	auth.mu.Unlock()
	v = cache.get(t.Context(), auth, false)
	if len(v.Limits) != 0 || v.ResetsKnown || !v.Updated.IsZero() || v.Account.ID != "account-two" {
		t.Fatal("old account data leaked")
	}
	fail = false
	switchAccount = true
	if v = cache.get(context.Background(), auth, true); len(v.Limits) != 0 || v.Account.ID != "account-three" || v.Error == "" {
		t.Fatal("account switched during refresh")
	}
}

func TestCodexUsagePanelAndManualButton(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	seedConnection(t, provider{Name: "codex", Type: "codex"}, "acct-a")
	seedConnection(t, provider{Name: "work", Type: "codex", AuthID: testAuthB}, "acct-b")
	providers := []provider{{Name: "codex", Type: "codex", BaseURL: CodexBaseURL}, {Name: "work", Type: "codex", BaseURL: CodexBaseURL, AuthID: testAuthB}}
	u := newUIServer(history.New(10, ""), NewStore(config{Local: localSetup{Providers: providers}}, ""), newHealth(""))
	calls := 0
	var accounts []string
	u.codexUsage.client = &http.Client{Transport: usageTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		accounts = append(accounts, r.Header.Get("ChatGPT-Account-Id"))
		return usageResponse(200, `{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":18000}},"rate_limit_reset_credits":{"available_count":0}}`), nil
	})}
	request := func(method, name, origin string) (int, string) {
		r := httptest.NewRequest(method, "http://localhost:8788/settings/codex/usage?provider="+name, nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		u.handler().ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	if code, html := request("GET", "work", ""); code != 200 || calls != 0 || !strings.Contains(html, "person@example.test") || !strings.Contains(html, `class="codex-usage"`) || !strings.Contains(html, `"provider":"work"`) || strings.Contains(html, "every 60s") ||
		!strings.Contains(html, "при старте роутера, раз в 10 минут и по кнопке") {
		t.Fatalf("GET panel: %d %s", code, html)
	}
	if code, _ := request("POST", "work", "https://other.test"); code != 403 || calls != 0 {
		t.Fatal("cross-origin refresh allowed")
	}
	if code, html := request("POST", "work", "http://localhost:8788"); code != 200 || calls != 1 || !strings.Contains(html, "80%") || !strings.Contains(html, "Доступно сбросов лимита: <b>0</b>") || strings.Contains(html, "private-refresh") {
		t.Fatalf("POST panel: %d %s", code, html)
	}
	if code, _ := request("POST", "codex", "http://localhost:8788"); code != 200 || calls != 2 {
		t.Fatalf("POST codex panel: %d", code)
	}
	if strings.Join(accounts, ",") != "acct-b,acct-a" {
		t.Fatalf("usage accounts: %v", accounts)
	}
	if a, b := u.usageCache(providers[0]).view.Account.ID, u.usageCache(providers[1]).view.Account.ID; a != "acct-a" || b != "acct-b" {
		t.Fatalf("views: %q %q", a, b)
	}
}

// The background refresh sends the refresh button's request once at start and
// once per tick. A failed request is not retried before the next tick, keeps
// the last good numbers and does not stop the loop.
func TestCodexUsageBackgroundRefresh(t *testing.T) {
	auth := &codexAuthStore{loaded: true, credential: usageCredential("acct")}
	calls := make(chan struct{}, 100)
	var fail atomic.Bool
	cache := &codexUsageCache{client: &http.Client{Transport: usageTransport(func(r *http.Request) (*http.Response, error) {
		calls <- struct{}{}
		if r.URL.String() != codexUsageURL {
			t.Errorf("request to %s", r.URL)
		}
		if fail.Load() {
			return usageResponse(503, "private-body"), nil
		}
		return usageResponse(200, `{"rate_limit":{"primary_window":{"used_percent":25}}}`), nil
	})}}
	// The clock moves a second at every read: Windows' clock ticks too coarsely
	// to tell two requests apart.
	start, reads := time.Now(), atomic.Int64{}
	cache.now = func() time.Time { return start.Add(time.Duration(reads.Add(1)) * time.Second) }
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		refreshCodexUsageEvery(ctx, func() []codexUsageTarget { return []codexUsageTarget{{"codex", cache, auth}} }, ticks)
		close(done)
	}()
	// A request holds the cache until its answer is stored, so a read right
	// after it sees the result. Each expected request is taken from calls
	// here; anything left there at the end is a request too many.
	request := func() codexUsageView {
		t.Helper()
		select {
		case <-calls:
		case <-time.After(5 * time.Second):
			t.Fatal("no request")
		}
		return cache.get(t.Context(), auth, false)
	}
	tick := func() { ticks <- time.Now() }

	v := request()
	if v.Updated.IsZero() || len(v.Limits) != 1 || v.Limits[0].Remaining != 75 {
		t.Fatalf("start: %+v", v)
	}
	updated := v.Updated

	fail.Store(true)
	tick()
	if v = request(); v.Error == "" || !v.Updated.Equal(updated) || len(v.Limits) != 1 || strings.Contains(v.Error, "private-body") {
		t.Fatalf("failed refresh: %+v", v)
	}
	tick()
	request()

	fail.Store(false)
	tick()
	if v = request(); v.Error != "" || !v.Updated.After(updated) {
		t.Fatalf("recovery: %+v", v)
	}

	// Without a Codex login there is nothing to ask for.
	auth.mu.Lock()
	auth.loaded, auth.path = false, filepath.Join(t.TempDir(), "missing.json")
	auth.mu.Unlock()
	tick()
	tick()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh loop did not stop")
	}
	if n := len(calls); n != 0 {
		t.Fatalf("%d requests beyond one at start and one per tick", n)
	}
}

// The background refresh asks for every Codex connection of the current
// settings, in providers order, and stores the answer where that
// connection's settings block reads it.
func TestCodexUsageRefreshEveryConnection(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	seedConnection(t, provider{Name: "codex", Type: "codex"}, "acct-a")
	seedConnection(t, provider{Name: "work", Type: "codex", AuthID: testAuthB}, "acct-b")
	providers := []provider{{Name: "codex", Type: "codex", BaseURL: CodexBaseURL}, {Name: "work", Type: "codex", BaseURL: CodexBaseURL, AuthID: testAuthB}}
	u := newUIServer(history.New(10, ""), NewStore(config{Local: localSetup{Providers: providers}}, ""), newHealth(""))
	calls := make(chan string, 10)
	u.codexUsage.client = &http.Client{Transport: usageTransport(func(r *http.Request) (*http.Response, error) {
		calls <- r.Header.Get("ChatGPT-Account-Id")
		return usageResponse(200, `{"rate_limit":{"primary_window":{"used_percent":25}}}`), nil
	})}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { refreshCodexUsageEvery(ctx, u.codexUsageTargets, make(chan time.Time)); close(done) }()

	accounts := []string{"acct-a", "acct-b"}
	for _, want := range accounts {
		select {
		case got := <-calls:
			if got != want {
				t.Fatalf("request for %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no request")
		}
	}
	for i, p := range providers {
		store, err := codexStoreFor(p)
		if err != nil {
			t.Fatal(err)
		}
		if v := u.usageCache(p).get(t.Context(), store, false); v.Updated.IsZero() || v.Account.ID != accounts[i] || len(v.Limits) != 1 {
			t.Fatalf("%s: %+v", p.Name, v)
		}
	}
	cancel()
	<-done
	if n := len(calls); n != 0 {
		t.Fatalf("%d requests beyond one per connection at start", n)
	}
}
