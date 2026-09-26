package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"localrouter/internal/history"
	"localrouter/internal/limits"
)

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return &buf
}

// countingTransport counts every outbound request the process makes through
// the default transport, which the reverse proxy uses.
type countingTransport struct {
	next http.RoundTripper
	n    atomic.Int64
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.next.RoundTrip(r)
}

func countOutbound(t *testing.T) *countingTransport {
	t.Helper()
	prev := http.DefaultTransport
	ct := &countingTransport{next: prev}
	http.DefaultTransport = ct
	t.Cleanup(func() { http.DefaultTransport = prev })
	return ct
}

// limitsRouter is the API handler with sonnet sent to Anthropic at upstream
// and opus/high to the local provider at local.
func limitsRouter(t *testing.T, upstream, local string) (*uiServer, http.Handler) {
	t.Helper()
	return limitsRouterStore(t, upstream, local, history.New(10, ""))
}

func limitsRouterStore(t *testing.T, upstream, local string, st *history.Store) (*uiServer, http.Handler) {
	t.Helper()
	cs, h, path := profileFixture(t)
	up, _ := url.Parse(upstream)
	c := cs.Get()
	c.Upstream = up
	cs = NewStore(c, path)
	l := cs.Get().Local.Clone()
	l.FamilyRoutes["sonnet"] = map[string]modelRoute{"default": {Mode: "anthropic"}}
	if local != "" {
		l.Providers[0].BaseURL = local
	}
	if err := cs.ApplyLocal(l, true); err != nil {
		t.Fatal(err)
	}
	u := newUIServer(st, cs, h)
	return u, newMainHandler(cs.Get(), cs, st, h, u)
}

// A streamed Anthropic response reaches the client unchanged and unbuffered,
// the router makes exactly the one request the client asked for, and the
// response headers land in the snapshot.
func TestAnthropicLimitsProxyIsTransparent(t *testing.T) {
	captureLog(t)
	release := make(chan struct{})
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Anthropic-Ratelimit-Example-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "41")
		w.Header().Set("Request-Id", "req_upstream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		_, _ = io.WriteString(w, "event: message_stop\ndata: {}\n\n")
	}))
	defer upstream.Close()
	out := countOutbound(t)

	u, handler := limitsRouter(t, upstream.URL, "")
	router := httptest.NewServer(handler)
	defer router.Close()

	body := `{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", router.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-token")
	resp, err := (&http.Client{Transport: &http.Transport{}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for k, v := range map[string]string{
		"Content-Type":                           "text/event-stream",
		"Anthropic-Ratelimit-Example-Status":     "allowed",
		"Anthropic-Ratelimit-Requests-Remaining": "41",
		"Request-Id":                             "req_upstream",
	} {
		if got := resp.Header.Values(k); len(got) != 1 || got[0] != v {
			t.Fatalf("client header %s = %v, want %q", k, got, v)
		}
	}
	rd := bufio.NewReader(resp.Body)
	first, err := rd.ReadString('\n')
	if err != nil || first != "event: message_start\n" {
		t.Fatalf("first event before upstream finished: %q %v", first, err)
	}
	close(release)
	rest, _ := io.ReadAll(rd)
	if got := first + string(rest); got != "event: message_start\ndata: {}\n\nevent: message_stop\ndata: {}\n\n" {
		t.Fatalf("body changed: %q", got)
	}
	if n := upstreamCalls.Load(); n != 1 {
		t.Fatalf("upstream saw %d requests", n)
	}
	if n := out.n.Load(); n != 1 {
		t.Fatalf("router made %d outbound requests, want 1", n)
	}
	v := u.limits.View(time.Now())
	if v.State != "fresh" || limitKeys(v) != "example,requests" || v.Windows[0].Status != "allowed" || v.Windows[1].Remaining == nil || *v.Windows[1].Remaining != 41 {
		t.Fatalf("snapshot %+v", v)
	}
}

// limitKeys is a view's parsed window names and raw header names, sorted:
// what the tests below compare to tell snapshots apart.
func limitKeys(v limits.View) string {
	var keys []string
	for _, w := range v.Windows {
		keys = append(keys, w.Name)
	}
	for k := range v.Raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// Only Anthropic's answers to /v1/messages count: a local route never reaches
// the proxy, and other proxied paths say nothing about the subscription.
func TestAnthropicLimitsIgnoresLocalAndOtherPaths(t *testing.T) {
	captureLog(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Example-Status", "allowed")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"input_tokens":1}`)
	}))
	defer upstream.Close()
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Example-Status", "local")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer local.Close()

	u, handler := limitsRouter(t, upstream.URL, local.URL)
	send := func(path, body string) int {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
		return w.Code
	}
	if code := send("/v1/messages", `{"model":"claude-opus-5","output_config":{"effort":"high"},"messages":[{"role":"user","content":"hi"}]}`); code != http.StatusServiceUnavailable {
		t.Fatalf("local route: %d", code)
	}
	if code := send("/v1/messages/count_tokens", `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`); code != http.StatusOK {
		t.Fatalf("count_tokens: %d", code)
	}
	if code := send("/v1/models", ""); code != http.StatusOK {
		t.Fatalf("catch-all: %d", code)
	}
	if v := u.limits.View(time.Now()); v.State != "unavailable" {
		t.Fatalf("non-messages or local response updated the snapshot: %+v", v)
	}
}

func limitsHeader(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

// /api/limits and the settings page show this JSON; docs/features.md
// describes it, so a change here is a change there.
func TestAnthropicLimitsViewJSON(t *testing.T) {
	captureLog(t)
	now := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	at := now.Format(time.RFC3339)
	l := limits.New("", limits.MaxAge)
	l.Observe(limitsHeader(
		"Anthropic-Ratelimit-Unified-5h-Utilization", "0.23",
		"Anthropic-Ratelimit-Unified-5h-Reset", "1790348400",
		"Anthropic-Ratelimit-Unified-Status", "allowed",
		"Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour",
	), now)
	report := func(l *limits.Store, at time.Time) string {
		got, _ := json.Marshal(limitsReportOf(l.View(at), nil, at))
		return string(got)
	}
	const noCodex = `{"source":"codex","state":"not_connected","max_age_seconds":1800}`
	for _, c := range []struct {
		after time.Duration
		want  string
	}{
		{90 * time.Second, `{"sources":[{"source":"anthropic","state":"fresh","observed_at":"` + at + `","age_seconds":90,"max_age_seconds":1800,` +
			`"raw":{"anthropic-ratelimit-unified-representative-claim":"five_hour"}},` + noCodex + `],` +
			`"windows":[{"source":"anthropic","name":"unified","status":"allowed","observed_at":"` + at + `"},` +
			`{"source":"anthropic","name":"unified-5h","remaining_percent":77,"used_percent":23,"reset_at":"2026-09-25T15:00:00Z","observed_at":"` + at + `"}]}`},
		{31 * time.Minute, `{"sources":[{"source":"anthropic","state":"unavailable","observed_at":"` + at + `","age_seconds":1860,"max_age_seconds":1800},` + noCodex + `],"windows":[]}`},
	} {
		if got := report(l, now.Add(c.after)); got != c.want {
			t.Fatalf("after %s\n got %s\nwant %s", c.after, got, c.want)
		}
	}

	// While fresh, raw is always there, even empty; windows always are.
	for _, c := range []struct {
		h    http.Header
		want string
	}{
		{limitsHeader("Anthropic-Ratelimit-Unified-5h-Utilization", "0.5"),
			`"raw":{}},` + noCodex + `],"windows":[{"source":"anthropic","name":"unified-5h","remaining_percent":50,"used_percent":50,"observed_at":"` + at + `"}]}`},
		{limitsHeader("Anthropic-Ratelimit-Unified-Fallback", "available"),
			`"raw":{"anthropic-ratelimit-unified-fallback":"available"}},` + noCodex + `],"windows":[]}`},
	} {
		l := limits.New("", limits.MaxAge)
		l.Observe(c.h, now)
		if got := report(l, now); !strings.HasPrefix(got, `{"sources":[{"source":"anthropic","state":"fresh",`) || !strings.HasSuffix(got, c.want) {
			t.Fatalf("got %s, want suffix %s", got, c.want)
		}
	}
	none := limits.New("", limits.MaxAge)
	none.Observe(http.Header{}, now)
	if got := report(none, now); got != `{"sources":[{"source":"anthropic","state":"no_headers","observed_at":"`+at+`","age_seconds":0,"max_age_seconds":1800},`+noCodex+`],"windows":[]}` {
		t.Fatalf("no headers: %s", got)
	}
}

// Codex windows come from the last successful usage request, in the same
// shape as Anthropic's, and only while that request is at most 30 minutes old.
func TestLimitsReportCodex(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var payload codexUsagePayload
	if err := json.Unmarshal([]byte(`{"plan_type":"pro",
		"rate_limit":{"allowed":true,"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_at":1790348400},
			"secondary_window":{"limit_window_seconds":604800}},
		"additional_rate_limits":[{"limit_name":"code_review","rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"limit_window_seconds":18000}}}]}`), &payload); err != nil {
		t.Fatal(err)
	}
	updated := now.Add(-2 * time.Minute)
	at := updated.Format(time.RFC3339)
	fresh := decodeCodexUsage(payload, codexAccount{ID: "acct"}, updated)
	codex := func(v codexUsageView, at time.Time) string {
		r := limitsReportOf(limits.View{State: "unavailable", MaxAgeSeconds: 1800}, []codexUsageView{v}, at)
		sources, _ := json.Marshal(r.Sources[1])
		windows, _ := json.Marshal(r.Windows)
		return string(sources) + " " + string(windows)
	}

	want := `{"source":"codex","state":"fresh","observed_at":"` + at + `","age_seconds":120,"max_age_seconds":1800} ` +
		`[{"source":"codex","name":"primary","remaining_percent":80,"used_percent":20,"reset_at":"2026-09-25T15:00:00Z","window_seconds":18000,"observed_at":"` + at + `"},` +
		`{"source":"codex","name":"secondary","window_seconds":604800,"observed_at":"` + at + `"},` +
		`{"source":"codex","name":"code_review-primary","remaining_percent":0,"used_percent":100,"status":"rejected","window_seconds":18000,"observed_at":"` + at + `"}]`
	if got := codex(fresh, now); got != want {
		t.Fatalf("fresh\n got %s\nwant %s", got, want)
	}

	// A failed refresh keeps the last good numbers while they are fresh and
	// says why the newer ones are missing.
	failed := fresh
	failed.Error = "сервис лимитов Codex вернул HTTP 503"
	want = `{"source":"codex","state":"fresh","observed_at":"` + at + `","age_seconds":120,"max_age_seconds":1800,"error":"сервис лимитов Codex вернул HTTP 503"} [`
	if got := codex(failed, now); !strings.HasPrefix(got, want) {
		t.Fatalf("failed refresh\n got %s\nwant prefix %s", got, want)
	}

	for _, c := range []struct {
		name string
		v    codexUsageView
		at   time.Time
		want string
	}{
		{"stale", failed, updated.Add(31 * time.Minute),
			`{"source":"codex","state":"unavailable","observed_at":"` + at + `","age_seconds":1860,"max_age_seconds":1800,"error":"сервис лимитов Codex вернул HTTP 503"} []`},
		{"never fetched", codexUsageView{Connected: true, Error: "нет ответа"}, now,
			`{"source":"codex","state":"unavailable","max_age_seconds":1800,"error":"нет ответа"} []`},
		{"not connected", codexUsageView{}, now,
			`{"source":"codex","state":"not_connected","max_age_seconds":1800} []`},
	} {
		if got := codex(c.v, c.at); got != c.want {
			t.Fatalf("%s\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// Limit values live in limits.json and the view only: neither the request
// history nor the log gets them.
func TestAnthropicLimitsValuesStayOutOfHistoryAndLog(t *testing.T) {
	buf := captureLog(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.4242")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "1790348417")
		w.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", "claim-value-7931")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	histPath := filepath.Join(t.TempDir(), "history.jsonl")
	u, handler := limitsRouterStore(t, upstream.URL, "", history.New(10, histPath))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if v := u.limits.View(time.Now()); v.State != "fresh" || limitKeys(v) != "anthropic-ratelimit-unified-representative-claim,unified-5h" {
		t.Fatalf("snapshot %+v", v)
	}
	data, err := os.ReadFile(histPath)
	if err != nil || len(data) == 0 {
		t.Fatalf("history not written: %v", err)
	}
	// Start and End carry fractional seconds: at hh:mm:57.6… they would
	// contain "57.6" without any limit value in the record.
	data = regexp.MustCompile(`"(Start|End)":"[^"]*"`).ReplaceAll(data, nil)
	for _, value := range []string{"0.4242", "1790348417", "claim-value-7931", "42.4", "57.6"} {
		if bytes.Contains(data, []byte(value)) {
			t.Fatalf("limit value %s in history: %s", value, data)
		}
		if strings.Contains(buf.String(), value) {
			t.Fatalf("limit value %s in log: %s", value, buf.String())
		}
	}
}

func TestLimitsPath(t *testing.T) {
	t.Setenv("ROUTER_ANTHROPIC_LIMITS_FILE", "/x/limits.json")
	if p := limitsPath(); p != "/x/limits.json" {
		t.Fatal(p)
	}
	t.Setenv("ROUTER_ANTHROPIC_LIMITS_FILE", "")
	if p := limitsPath(); p != "" {
		t.Fatalf("empty override must disable persistence: %q", p)
	}
	os.Unsetenv("ROUTER_ANTHROPIC_LIMITS_FILE")
	exe, _ := os.Executable()
	if p := limitsPath(); p != filepath.Join(filepath.Dir(exe), "limits.json") {
		t.Fatal(p)
	}
}

// limitsSection is the Anthropic limits block of the settings page.
func limitsSection(t *testing.T, h http.Handler) string {
	t.Helper()
	body := get(t, h, "GET", "/settings", nil).Body.String()
	start := strings.Index(body, `<section class="settings-section" id="anthropic-limits">`)
	if start < 0 {
		t.Fatal("no Anthropic limits section on the settings page")
	}
	end := strings.Index(body[start:], "</section>")
	return body[start : start+end]
}

func TestAnthropicLimitsSettingsBlock(t *testing.T) {
	captureLog(t)
	u, h := testUI(t)
	page := get(t, h, "GET", "/settings", nil).Body.String()
	codex, block, conns := strings.Index(page, `id="codex-subscription"`), strings.Index(page, `id="anthropic-limits"`), strings.Index(page, `id="connections"`)
	if codex < 0 || codex > block || block > conns {
		t.Fatalf("limits block is not between Codex subscription and connections: %d %d %d", codex, block, conns)
	}
	if !strings.Contains(page, `<a href="#anthropic-limits">`) {
		t.Fatal("no navigation link to the limits block")
	}
	full := httptest.NewRecorder()
	h.ServeHTTP(full, httptest.NewRequest("GET", "/settings", nil))
	if full.Code != http.StatusOK || !strings.Contains(full.Body.String(), `id="anthropic-limits"`) || strings.Contains(full.Body.String(), "<no value>") {
		t.Fatalf("full settings page: %d", full.Code)
	}

	now := time.Now()
	reset := now.Add(90*time.Minute + 30*time.Second).Truncate(time.Second)
	withValues := limitsHeader(
		"Anthropic-Ratelimit-Unified-5h-Utilization", "0.23",
		"Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(reset.Unix(), 10),
		"Anthropic-Ratelimit-Unified-7d-Utilization", "0.955",
		"Anthropic-Ratelimit-Unified-Status", "allowed",
		"Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour",
	)
	for _, c := range []struct {
		name       string
		setup      func(*limits.Store)
		want, deny []string
	}{
		{"nothing observed", func(*limits.Store) {},
			[]string{"Недоступно", "ещё не видел ответов Anthropic"}, []string{"осталось"}},
		{"no headers", func(l *limits.Store) { l.Observe(http.Header{}, now) },
			[]string{"Anthropic не присылает лимиты в ответах"}, []string{"Недоступно", "осталось"}},
		{"fresh", func(l *limits.Store) { l.Observe(withValues, now) },
			[]string{"<h4>unified-5h</h4>", "77% <span>осталось</span>", "Использовано 23%", `<progress max="100" value="77"`,
				`Сброс: <time datetime="` + reset.UTC().Format(time.RFC3339) + `"`, "через 1 ч. 30 мин.",
				"4.5% <span>осталось</span>", "quota-low", "Статус: <code>allowed</code>", "Данные получены",
				`<details class="settings-extra">`, "<code>anthropic-ratelimit-unified-representative-claim</code>", "five_hour"},
			[]string{"Недоступно", "<no value>", "%!"}},
		{"unknown headers only", func(l *limits.Store) {
			l.Observe(limitsHeader("Anthropic-Ratelimit-Unified-Fallback", "available"), now)
		},
			[]string{"Окон лимитов в заголовках нет", "<code>anthropic-ratelimit-unified-fallback</code>", "available"},
			[]string{"Недоступно", "<progress", "<no value>"}},
		{"stale", func(l *limits.Store) { l.Observe(withValues, now.Add(-31*time.Minute)) },
			[]string{"Недоступно", "старше 30 мин"}, []string{"осталось", "unified-5h", "five_hour", "<progress"}},
	} {
		u.limits = limits.New("", limits.MaxAge)
		c.setup(u.limits)
		s := limitsSection(t, h)
		for _, w := range c.want {
			if !strings.Contains(s, w) {
				t.Fatalf("%s: %q missing in\n%s", c.name, w, s)
			}
		}
		for _, d := range c.deny {
			if strings.Contains(s, d) {
				t.Fatalf("%s: %q must not appear in\n%s", c.name, d, s)
			}
		}
		if !strings.Contains(s, "Сам роутер к Anthropic не обращается") || strings.Contains(s, "<button") || strings.Contains(s, "hx-") {
			t.Fatalf("%s: the block must be passive and say so:\n%s", c.name, s)
		}
	}
}

func TestAnthropicLimitsAPI(t *testing.T) {
	captureLog(t)
	u, h := testUI(t)
	out := countOutbound(t)
	now := time.Now()
	u.limits.Observe(limitsHeader(
		"Anthropic-Ratelimit-Unified-5h-Utilization", "0.23",
		"Anthropic-Ratelimit-Unified-5h-Reset", "1790348400",
		"Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour",
	), now)

	// Codex is connected and its last refresh failed: its source says so and
	// the Anthropic part is unaffected.
	codex := provider{Name: "codex", Type: "codex", BaseURL: CodexBaseURL}
	c := u.cs.Get()
	c.Local.Providers = append(c.Local.Providers, codex)
	u.cs = NewStore(c, "")
	oldAuth := codexAuth
	codexAuth = &codexAuthStore{loaded: true, credential: usageCredential("acct")}
	t.Cleanup(func() { codexAuth = oldAuth })
	var codexCalls atomic.Int64
	u.codexUsage.client = &http.Client{Transport: usageTransport(func(*http.Request) (*http.Response, error) {
		codexCalls.Add(1)
		return usageResponse(503, "private-body"), nil
	})}
	u.usageCache(codex).get(t.Context(), codexAuth, true)

	w := get(t, h, "GET", "/api/limits", nil)
	if n := out.n.Load(); n != 0 || codexCalls.Load() != 1 {
		t.Fatalf("GET /api/limits made %d outbound requests, %d to Codex", n, codexCalls.Load()-1)
	}
	if ct, cc := w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"); ct != "application/json" || cc != "no-store" {
		t.Fatalf("Content-Type %q, Cache-Control %q", ct, cc)
	}
	var got limitsReport
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := limitsReportOf(u.limits.View(now), nil, now)
	gotWindows, _ := json.Marshal(got.Windows)
	wantWindows, _ := json.Marshal(want.Windows)
	if len(got.Sources) != 2 || got.Sources[0].Source != "anthropic" || got.Sources[0].State != "fresh" || !got.Sources[0].ObservedAt.Equal(want.Sources[0].ObservedAt) ||
		got.Sources[0].AgeSeconds == nil || got.Sources[0].MaxAgeSeconds != 1800 || !maps.Equal(got.Sources[0].Raw, want.Sources[0].Raw) ||
		string(gotWindows) != string(wantWindows) || len(got.Windows) != 1 {
		t.Fatalf("JSON %s, store %+v", w.Body.String(), want)
	}
	if c := got.Sources[1]; c.Source != "codex" || c.Provider != "codex" || c.State != "unavailable" || c.Error == "" || strings.Contains(w.Body.String(), "private-body") {
		t.Fatalf("codex source: %s", w.Body.String())
	}
	if s := limitsSection(t, h); !strings.Contains(s, "77% <span>осталось</span>") || !strings.Contains(s, "five_hour") {
		t.Fatalf("page disagrees with JSON:\n%s", s)
	}

	for _, method := range []string{"POST", "PUT", "DELETE"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/api/limits", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /api/limits: %d", method, rec.Code)
		}
	}

	u.limits = limits.New("", limits.MaxAge)
	codexAuth = &codexAuthStore{path: filepath.Join(t.TempDir(), "missing.json")}
	if body := get(t, h, "GET", "/api/limits", nil).Body.String(); !strings.HasPrefix(body,
		`{"sources":[{"source":"anthropic","state":"unavailable","max_age_seconds":1800},{"source":"codex","provider":"codex","state":"not_connected","max_age_seconds":1800}],"windows":[]}`) {
		t.Fatalf("signed out: %s", body)
	}
	// With no Codex connection at all the report still has one codex source.
	c = u.cs.Get()
	c.Local.Providers = c.Local.Providers[:len(c.Local.Providers)-1]
	u.cs = NewStore(c, "")
	if body := get(t, h, "GET", "/api/limits", nil).Body.String(); !strings.HasPrefix(body,
		`{"sources":[{"source":"anthropic","state":"unavailable","max_age_seconds":1800},{"source":"codex","state":"not_connected","max_age_seconds":1800}],"windows":[]}`) {
		t.Fatalf("nothing observed: %s", body)
	}
}

// Every Codex connection is its own codex source in /api/limits, named by
// provider and listed in providers order; its windows carry the same name.
func TestCodexLimitsPerConnection(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	seedConnection(t, provider{Name: "codex", Type: "codex"}, "acct-a")
	seedConnection(t, provider{Name: "work", Type: "codex", AuthID: testAuthB}, "acct-b")
	providers := []provider{{Name: "codex", Type: "codex", BaseURL: CodexBaseURL}, {Name: "work", Type: "codex", BaseURL: CodexBaseURL, AuthID: testAuthB}}
	u := newUIServer(history.New(10, ""), NewStore(config{Local: localSetup{Providers: providers}}, ""), newHealth(""))
	u.limits = limits.New("", limits.MaxAge)
	used := map[string]string{"acct-a": "20", "acct-b": "70"}
	u.codexUsage.client = &http.Client{Transport: usageTransport(func(r *http.Request) (*http.Response, error) {
		return usageResponse(200, `{"rate_limit":{"primary_window":{"used_percent":`+used[r.Header.Get("ChatGPT-Account-Id")]+`}}}`), nil
	})}
	for _, p := range providers {
		store, err := codexStoreFor(p)
		if err != nil {
			t.Fatal(err)
		}
		u.usageCache(p).get(t.Context(), store, true)
	}

	body := get(t, u.handler(), "GET", "/api/limits", nil).Body.Bytes()
	var got limitsReport
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	var sources, windows []string
	for _, s := range got.Sources {
		sources = append(sources, s.Source+"/"+s.Provider+"/"+s.State)
	}
	for _, w := range got.Windows {
		percent := "-"
		if w.UsedPercent != nil {
			percent = fmt.Sprint(*w.UsedPercent)
		}
		windows = append(windows, w.Source+"/"+w.Provider+"/"+w.Name+"/"+percent)
	}
	if strings.Join(sources, ",") != "anthropic//unavailable,codex/codex/fresh,codex/work/fresh" ||
		strings.Join(windows, ",") != "codex/codex/primary/20,codex/work/primary/70" {
		t.Fatalf("sources %v, windows %v\n%s", sources, windows, body)
	}
}

// TestCompatAPILimits is the /api/limits part of the compatibility check of
// internal/limits: the body built from limits.json, read by the moved store,
// and fixed Codex views is the one the code before the move wrote to
// golden/api-limits.json, byte for byte:
//
//	go test -run TestCompatAPILimits -count=1 .
//	ROUTER_COMPAT_DIR=<anonymized live copy> go test -run TestCompatAPILimits -count=1 .
//
// ROUTER_COMPAT_DIR is the copy the compat tests in internal/ read too.
func TestCompatAPILimits(t *testing.T) {
	dirs := []string{filepath.Join("testdata", "compat")}
	if d := os.Getenv("ROUTER_COMPAT_DIR"); d != "" {
		dirs = append(dirs, d)
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			home := filepath.Join(dir, "home")
			now := compatNow(t, home)
			// Nothing is observed, so the store only reads limits.json.
			l := limits.New(filepath.Join(home, "limits.json"), limits.MaxAge)
			var got bytes.Buffer
			if err := json.NewEncoder(&got).Encode(limitsReportOf(l.View(now), compatCodex(now), now)); err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join(dir, "golden", "api-limits.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				// Offsets only: the limits of a live copy stay out of the test log.
				at := 0
				for at < got.Len() && at < len(want) && got.Bytes()[at] == want[at] {
					at++
				}
				t.Errorf("api-limits.json differs from golden at byte %d: %d bytes, want %d", at, got.Len(), len(want))
			}
		})
	}
}

// compatNow is a minute after the newest observation in limits.json, so the
// report does not depend on the wall clock.
func compatNow(t *testing.T, home string) time.Time {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "limits.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		WithHeaders *struct {
			At time.Time `json:"at"`
		} `json:"with_headers"`
		WithoutAt time.Time `json:"without_at"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	at := f.WithoutAt
	if f.WithHeaders != nil && f.WithHeaders.At.After(at) {
		at = f.WithHeaders.At
	}
	if at.IsZero() {
		t.Fatal("limits.json has no observation")
	}
	return at.Add(time.Minute)
}

// compatCodex covers each Codex source state /api/limits reports.
func compatCodex(now time.Time) []codexUsageView {
	return []codexUsageView{
		{Provider: "codex-a", Connected: true, Plan: "pro", Updated: now.Add(-2 * time.Minute), Attempted: now.Add(-2 * time.Minute),
			Limits: []codexUsageRow{
				{Name: "5h", ID: "primary", Seconds: 18000, Known: true, Remaining: 83, Used: 17, Reset: now.Add(time.Hour)},
				{Name: "weekly", ID: "secondary", Seconds: 604800, Known: true, Remaining: 5, Used: 95, Blocked: true},
			}},
		{Provider: "codex-b", Connected: true, Error: "no response"},
		{Provider: "codex-c", Connected: true, Updated: now.Add(-time.Hour), Limits: []codexUsageRow{{ID: "primary"}}},
		{},
	}
}
