package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSettingsUsesAnthropicPools(t *testing.T) {
	u, h := testUI(t)
	u.cs.provPath = filepath.Join(t.TempDir(), "providers.json")
	body := get(t, h, "GET", "/settings", nil).Body.String()
	for _, model := range []string{"claude-opus-5-5", "claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5"} {
		if !strings.Contains(body, `data-model="`+model+`"`) {
			t.Errorf("missing editable pool for %s", model)
		}
	}
	for _, obsolete := range []string{"всё локально", "сделать локальной", "сделать основной", "Локальные провайдеры"} {
		if strings.Contains(body, obsolete) {
			t.Errorf("obsolete routing UI: %s", obsolete)
		}
	}
	get(t, h, "POST", "/settings/pools", url.Values{"op": {"create"}, "name": {"Сложные задачи"}})
	get(t, h, "POST", "/settings/pools", url.Values{"op": {"add"}, "name": {"Сложные задачи"}, "key": {"p/m1"}, "effort": {"high"}})
	pool := u.cs.get().local.ModelPools["Сложные задачи"]
	if len(pool) != 1 || pool[0].Model != "p/m1" || pool[0].Effort != "high" {
		t.Fatalf("pool not saved: %+v", pool)
	}
	get(t, h, "POST", "/settings/route", url.Values{"model": {"claude-opus-5"}, "high": {"pool:Сложные задачи"}, "low": {"anthropic"}})
	cfg := u.cs.get()
	if cfg.routeFor("claude-opus-5", "high").Pool != "Сложные задачи" || cfg.routeFor("claude-opus-5", "low").Mode != "anthropic" {
		t.Fatal("routes not saved")
	}
	if cfg.routeFor("claude-sonnet-5", "high").Mode != "disabled" || cfg.routeFor("claude-opus-5", "default").Mode != "disabled" {
		t.Fatal("unassigned routes enabled")
	}
	get(t, h, "POST", "/settings/pools", url.Values{"op": {"delete"}, "name": {"Сложные задачи"}})
	if _, ok := u.cs.get().local.ModelPools["Сложные задачи"]; !ok {
		t.Fatal("deleted a referenced pool")
	}
	// A normal navigation must render settings as a full-width page too.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/settings", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `class="settings-page"`) || !strings.Contains(w.Body.String(), `data-model="claude-opus-5"`) {
		t.Fatal("settings page not rendered")
	}

}

// Templates are parsed at startup only; a stray {{end}} must fail here,
// not when launchd restarts the router.
func TestTemplatesParse(t *testing.T) {
	if _, err := uiTemplates(); err != nil {
		t.Fatal(err)
	}
}

// testUI builds a UI server over an in-memory history with one cloud and one
// local record per session, a provider that answers /models, and a finished
// response on every record so all detail views have something to show.
func testUI(t *testing.T, sessions ...string) (*uiServer, http.Handler) {
	t.Helper()
	prov := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"m1"},{"id":"m2"}]}`)
	}))
	t.Cleanup(prov.Close)
	cs := &configStore{c: config{local: oneProvider(prov.URL, "m1"), failover: true, firstByte: 45 * time.Second}}
	st := newStore(100, "")
	for _, sid := range sessions {
		meta := fmt.Sprintf(`{"metadata":{"user_id":"{\"session_id\":\"%s\"}"},"messages":[{"role":"user","content":"hello"}]}`, sid)
		for _, route := range []string{"cloud", "local"} {
			r := &record{Start: time.Now(), Model: "claude-x", Route: route, ReqBody: []byte(meta), Session: sid}
			if route == "local" {
				r.Served = "p/m1"
				r.OpenAIBody = []byte(`{"model":"m1","messages":[]}`)
			}
			st.add(r)
			rw := newRecorder(httptest.NewRecorder(), 1<<20)
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(200)
			rw.Write([]byte(`{"type":"message","content":[{"type":"text","text":"hi there"}],"stop_reason":"end_turn"}`))
			st.finish(r.ID, rw, nil)
		}
	}
	u := newUIServer(st, cs, newHealth(""))
	return u, u.handler()
}

func get(t *testing.T, h http.Handler, method, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("%s %s: %d %s", method, target, w.Code, firstLine(w.Body.String()))
	}
	if b := w.Body.String(); strings.Contains(b, "<no value>") {
		t.Fatalf("%s %s: <no value> in output", method, target)
	}
	return w
}

// Every page and partial must execute on real data: a bad field reference
// only fails at execution time, which parsing alone never catches.
func TestUIPagesRender(t *testing.T) {
	u, h := testUI(t, "s1", "s2")
	get(t, h, "GET", "/", nil)
	get(t, h, "GET", "/status", nil)
	get(t, h, "GET", "/requests", nil)
	get(t, h, "GET", "/requests?route=local&q=hi&model=claude", nil)
	for _, rec := range u.st.list() {
		for _, view := range []string{"structure", "sent", "response", "raw"} {
			get(t, h, "GET", "/requests/"+rec.ID+"?view="+view+"&q=hi", nil)
		}
	}
	get(t, h, "GET", "/settings", nil)
	get(t, h, "GET", "/settings/provider?name=p", nil)
	get(t, h, "GET", "/settings/provider?name=", nil)
	get(t, h, "POST", "/settings/probe", url.Values{"provider": {"p"}})
	get(t, h, "POST", "/settings/models", url.Values{"op": {"reset"}})
	if !strings.Contains(get(t, h, "GET", "/requests", nil).Body.String(), "→ p/m1") {
		t.Fatal("list does not show provider/model next to the requested model")
	}
}

func TestListCountsAndSessions(t *testing.T) {
	_, h := testUI(t, "s1", "s2", "s3") // 6 records in 3 sessions, newest first
	body := get(t, h, "GET", "/requests?limit=3", nil).Body.String()
	if !strings.Contains(body, "показано 3 из 6") || !strings.Contains(body, "сессий 2") {
		t.Fatalf("limit=3: %s", firstLine(body))
	}
	if n := strings.Count(body, `class="item`); n != 3 {
		t.Fatalf("limit=3 rendered %d items", n)
	}
	body = get(t, h, "GET", "/requests?session=s2", nil).Body.String()
	if !strings.Contains(body, "показано 2 из 6") || !strings.Contains(body, "сессий 1") || strings.Count(body, `data-sid="s2"`) != 1 {
		t.Fatalf("session filter: %s", firstLine(body))
	}
	body = get(t, h, "GET", "/requests?route=cloud", nil).Body.String()
	if !strings.Contains(body, "показано 3 из 6") || strings.Contains(body, `class="item local`) {
		t.Fatalf("route filter: %s", firstLine(body))
	}
}

func TestSettingsShowsModelStats(t *testing.T) {
	u, h := testUI(t)
	u.cs.provPath = filepath.Join(t.TempDir(), "providers.json")
	get(t, h, "POST", "/settings/pools", url.Values{"op": {"create"}, "name": {"work"}})
	get(t, h, "POST", "/settings/pools", url.Values{"op": {"add"}, "name": {"work"}, "key": {"p/m1"}})
	u.hl.recordProbe("p/m1", true, time.Second, "")
	u.hl.record("p/m1", true, 1500*time.Millisecond, "")
	u.hl.record("p/m1", false, 0, "boom")
	u.hl.noteFailover("p/m1", "p/m2")
	s := u.hl.snapshot("p/m1")

	body := get(t, h, "GET", "/settings", nil).Body.String()
	// The pool card and the model catalog both show the model's stats, so
	// look only at the model's row in each: the other must not stand in.
	rows := map[string]string{
		"pool member":   between(t, between(t, body, `data-pool="work"`, "pool-behavior"), "<code>p/m1</code>", "</div>"),
		"catalog model": between(t, between(t, body, "Каталог добавленных моделей", "</details>"), "<code>p/m1</code>", "</div>"),
	}
	for name, row := range rows {
		for _, want := range []string{
			"1 ответов", "1 сбоев", "подряд 1",
			fmt.Sprintf("рейтинг %d%%", s.ScorePct()),
			"пауза ещё", "ушла дальше 1", "проверки 1/0", "boom",
		} {
			if !strings.Contains(row, want) {
				t.Errorf("%s stats missing %q", name, want)
			}
		}
	}
}

// between returns s from the first start up to the first end after it.
func between(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("%q not rendered", start)
	}
	j := strings.Index(s[i:], end)
	if j < 0 {
		t.Fatalf("no %q after %q", end, start)
	}
	return s[i : i+j]
}

// withBadProvider returns the providers file with a Codex connection that the
// strict reader rejects: a second connection without auth_id.
func withBadProvider(t *testing.T, good []byte) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(good, &doc); err != nil {
		t.Fatal(err)
	}
	providers, _ := doc["providers"].([]any)
	doc["providers"] = append([]any{map[string]any{"name": "work", "type": "codex", "base_url": codexBaseURL}}, providers...)
	bad, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(bad)
}

func TestReloadErrorBanner(t *testing.T) {
	cs, _, path := profileFixture(t)
	u := newUIServer(newStore(10, ""), cs, newHealth(""))
	good, _ := os.ReadFile(path)
	bad := withBadProvider(t, good)
	writeRaw(t, path, bad)
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(path, future, future)
	cs.pollOnce()
	v := u.settingsView()
	if len(v.ReloadErrors) != 1 || !strings.Contains(v.ReloadErrors[0].Err, "auth_id") || v.ReloadErrors[0].Snapshot.IsZero() {
		t.Fatalf("banner: %+v", v.ReloadErrors)
	}
	if _, ok := cs.get().local.provider("work"); ok {
		t.Fatal("rejected file was applied")
	}
	if data, _ := os.ReadFile(path); string(data) != bad {
		t.Fatal("rejected file was rewritten")
	}
	cs.pollOnce()
	if again := u.settingsView().ReloadErrors; len(again) != 1 || !again[0].At.Equal(v.ReloadErrors[0].At) {
		t.Fatal("same error re-stamped or duplicated")
	}
	writeRaw(t, path, string(good))
	later := future.Add(2 * time.Second)
	os.Chtimes(path, later, later)
	cs.pollOnce()
	if left := u.settingsView().ReloadErrors; len(left) != 0 {
		t.Fatalf("banner not cleared: %+v", left)
	}
}

func TestReloadErrorBannerRenders(t *testing.T) {
	u, h := testUI(t)
	u.cs.noteReload("/x/providers.json", errors.New("boom"))
	body := get(t, h, "GET", "/settings", nil).Body.String()
	if !strings.Contains(body, "файл providers.json не применён: boom; действует снимок от") {
		t.Fatal("banner not rendered")
	}
}

func TestCodexSectionPerConnection(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	var calls atomic.Int32
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	// Connection cards probe model lists on render; only a usage request
	// spends the subscription and must wait for the button.
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == codexUsageURL {
			calls.Add(1)
		}
		return usageResponse(500, ""), nil
	})
	_, h := codexUI(t)
	work, _ := codexStoreFor(provider{Name: "work", Type: "codex", AuthID: testAuthB})
	if err := work.save(usageCredential("work-account")); err != nil {
		t.Fatal(err)
	}
	body := get(t, h, "GET", "/settings", nil).Body.String()
	for _, want := range []string{`<h4>codex</h4>`, `<h4>work</h4>`, `hx-get="/settings/codex/usage?provider=work"`, `hx-get="/settings/codex/status?provider=work"`, `name="provider" value="work"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(body, `id="codex-login"`) {
		t.Error("one element id repeated per connection")
	}
	if strings.Contains(body, "access_token") || strings.Contains(body, "private-refresh") {
		t.Fatal("token leaked into the page")
	}
	usage := get(t, h, "GET", "/settings/codex/usage?provider=work", nil).Body.String()
	for _, want := range []string{`class="codex-usage"`, `"provider":"work"`, `hx-target="closest .codex-usage"`} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage fragment missing %s", want)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("page load asked for usage %d times", calls.Load())
	}
}

// The provider pane's "import from Codex CLI" button names its provider;
// without it the import handler answers 400 after Task 3.
func TestCodexProviderPaneImportNamesProvider(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	_, h := codexUI(t)
	pane := get(t, h, "GET", "/settings/provider?name=work", nil).Body.String()
	if !strings.Contains(pane, `hx-post="/settings/codex/import" hx-vals='{"provider":"work"}'`) {
		t.Fatalf("import button without provider:\n%s", pane)
	}
	data, _ := json.Marshal(usageCredential("work-account"))
	writeRaw(t, codexAuth.cliPath, string(data))
	if code := get(t, h, "POST", "/settings/codex/import", url.Values{"provider": {"work"}}).Code; code != 200 {
		t.Fatalf("import: %d", code)
	}
	work, _ := codexStoreFor(provider{Name: "work", Type: "codex", AuthID: testAuthB})
	if !work.connected() || codexAuth.connected() {
		t.Fatal("import did not reach work's file")
	}
}
