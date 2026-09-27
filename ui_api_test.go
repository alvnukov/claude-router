package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"localrouter/internal/history"
	webui "localrouter/internal/ui"
)

func apiCall(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw := ""
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(data)
	}
	req := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(raw))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://localhost")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}
func TestUIJSONStateDoesNotProbeOrExposeCredentials(t *testing.T) {
	calls := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "unexpected probe", 500) }))
	defer remote.Close()
	u, _ := testUI(t)
	u.cs.c.local.Providers[0].BaseURL = remote.URL + "/v1?key=URL-SECRET"
	u.cs.c.local.Providers[0].APIKey = "API-SECRET"
	u.cs.c.local.Providers[0].AuthID = "AUTH-SECRET"
	w := apiCall(t, u.handler(), "GET", "/api/ui/state", nil)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	for _, secret := range []string{"API-SECRET", "URL-SECRET", "AUTH-SECRET", "api_key", "auth_id"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Errorf("state exposed %s", secret)
		}
	}
	if calls != 0 {
		t.Fatalf("state made %d remote calls", calls)
	}
	var state webui.State
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Connections[1].Name != "p" || !state.Connections[1].KeySet || state.Connections[1].BaseURL != remote.URL+"/v1" {
		t.Fatalf("invalid safe connection: %+v", state.Connections[1])
	}
}

func TestUIJSONHistoryErrorsHideCredentials(t *testing.T) {
	u, h := testUI(t)
	p := &u.cs.c.local.Providers[0]
	p.APIKey = "REVIEW-API-SECRET"
	p.BaseURL = "http://localhost:1/v1?key=REVIEW-URL-SECRET"
	message := "Post " + p.BaseURL + "/chat/completions: transport failed; key=" + p.APIKey
	rec := &history.Record{Start: time.Now(), Model: "claude-opus-5", Route: "local", Session: "review-session", Served: "p/m1", ReqBody: []byte(`{"model":"claude-opus-5","messages":[]}`)}
	u.st.Add(rec)
	rw := history.NewRecorder(httptest.NewRecorder(), 4096)
	writeAnthropicError(rw, 502, "api_error", message)
	u.st.Finish(rec.ID, rw, &history.Trace{Attempts: []history.Attempt{{Model: "p/m1", Err: message}}})
	for _, path := range []string{"/api/ui/state", "/api/ui/requests", "/api/ui/requests/" + rec.ID} {
		response := apiCall(t, h, "GET", path, nil)
		for _, secret := range []string{"REVIEW-API-SECRET", "REVIEW-URL-SECRET"} {
			if strings.Contains(response.Body.String(), secret) {
				t.Errorf("%s exposes a credential in an error", path)
			}
		}
		if !strings.Contains(response.Body.String(), "transport failed") {
			t.Errorf("%s lost the diagnostic message", path)
		}
	}
	if !strings.Contains(u.st.Get(rec.ID).Resp.Error, "REVIEW-URL-SECRET") {
		t.Fatal("display sanitization changed the original history record")
	}
}

func TestUIJSONDistinguishesDisabledAndAbsentRoutes(t *testing.T) {
	u, h := testUI(t)
	u.cs.c.local.FamilyRoutes = map[string]map[string]modelRoute{"sonnet": {"high": {Mode: "disabled"}}}
	u.cs.c.local.Routes = map[string]map[string]modelRoute{"claude-sonnet-custom": {"high": {Mode: "disabled"}}}
	var state webui.State
	if err := json.Unmarshal(apiCall(t, h, "GET", "/api/ui/state", nil).Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	for _, rows := range [][]webui.RouteRow{state.Families, state.Routes} {
		for _, row := range rows {
			if row.Model != "sonnet" && row.Model != "claude-sonnet-custom" {
				continue
			}
			for _, choice := range row.Choices {
				if choice.Configured != (choice.Effort == "high") {
					t.Errorf("%s/%s loses explicit rule presence", row.Model, choice.Effort)
				}
			}
		}
	}
}
func TestUIJSONRequestFiltersDetailsAndUTF8(t *testing.T) {
	u, h := testUI(t, "session-a", "session-b")
	r := &history.Record{Start: time.Now(), Model: "claude-opus-5", Route: "local", Session: "failed-session", Served: "p/m1", ReqBody: []byte(`{"system":"needle-in-system","messages":[{"role":"user","content":"` + strings.Repeat("Ж", 200) + `"}]}`), Headers: map[string]string{"Authorization": "Bearer PRIVATE", "X-Api-Key": "PRIVATE", "Cookie": "PRIVATE", "Content-Type": "application/json"}}
	u.st.Add(r)
	writer := history.NewRecorder(httptest.NewRecorder(), 1024)
	writer.WriteHeader(500)
	_, _ = writer.Write([]byte(`{"type":"error","error":{"message":"Не получилось"}}`))
	u.st.Finish(r.ID, writer, nil)
	w := apiCall(t, h, "GET", "/api/ui/requests?connection=p&errors=1&limit=1", nil)
	var list webui.RequestList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Total != 1 || len(list.Items) != 1 || !utf8.ValidString(list.Items[0].Preview) || len([]rune(list.Items[0].Preview)) != 141 {
		t.Fatalf("filtered request: %+v", list)
	}
	w = apiCall(t, h, "GET", "/api/ui/requests?q=needle-in-system&limit=9999", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if list.Total != 1 || list.Limit != 200 {
		t.Fatalf("full text/bounds: %+v", list)
	}
	w = apiCall(t, h, "GET", "/api/ui/requests/"+r.ID, nil)
	var detail webui.Detail
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.ID != r.ID || len(detail.Headers) != 1 || strings.Contains(w.Body.String(), "PRIVATE") {
		t.Fatal("detail lost identity or exposed authentication headers")
	}
	w = apiCall(t, h, "GET", "/api/ui/requests/missing", nil)
	if w.Code != 404 || !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatal("missing detail lacks JSON 404")
	}
}
func TestUIJSONRejectsReferencedConnectionInInactiveProfile(t *testing.T) {
	u, h := testUI(t)
	u.cs.provPath = filepath.Join(t.TempDir(), "providers.json")
	l := u.cs.c.local
	l.Profiles = map[string]routingProfile{"active": l.routing(), "other": {ModelPools: map[string][]poolTarget{"work": {{Model: "p/m1"}}}}}
	l.ActiveProfile = "active"
	u.cs.c.local = l
	w := apiCall(t, h, "POST", "/api/ui/actions", webui.Action{Action: "connection.edit", Fields: map[string]string{"op": "remove", "name": "p"}})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "other") || len(u.cs.get().local.Providers) != 1 {
		t.Fatalf("referenced delete: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(u.cs.provPath); !os.IsNotExist(err) {
		t.Fatal("rejected delete wrote configuration")
	}
}
func TestUIJSONSavesDisplayNameWithoutChangingIdentity(t *testing.T) {
	u, h := testUI(t)
	u.cs.provPath = filepath.Join(t.TempDir(), "providers.json")
	p := u.cs.c.local.Providers[0]
	p.APIKey = "unchanged-key"
	u.cs.c.local.Providers[0] = p
	w := apiCall(t, h, "POST", "/api/ui/actions", webui.Action{Action: "connection.edit", Fields: map[string]string{"op": "update", "orig": p.Name, "name": p.Name, "base_url": p.BaseURL, "display_name": "Рабочий аккаунт"}})
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	got := u.cs.get().local.Providers[0]
	if got.Name != p.Name || got.APIKey != p.APIKey || got.DisplayName != "Рабочий аккаунт" {
		t.Fatalf("identity changed: %+v", got)
	}
	saved, err := readProviders(u.cs.provPath)
	if err != nil || saved.Providers[0].DisplayName != got.DisplayName {
		t.Fatalf("display name not persisted: %v", err)
	}
	w = apiCall(t, h, "POST", "/api/ui/actions", webui.Action{Action: "connection.edit", Fields: map[string]string{"op": "add", "name": "invalid/name"}})
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("validation failure returned success: %d", w.Code)
	}
}
func TestUIJSONUsageInvalidatedAfterAccountChange(t *testing.T) {
	previous := codexAuth
	codexAuth = &codexAuthStore{path: filepath.Join(t.TempDir(), "codex-auth.json"), loaded: true}
	t.Cleanup(func() { codexAuth = previous })
	codexAuth.credential.Tokens.AccountID = "new-account"
	codexAuth.credential.AuthMode = "chatgpt"
	u, h := testUI(t)
	p := provider{Name: "codex", Type: "codex", BaseURL: codexBaseURL}
	u.cs.c.local.Providers = []provider{p}
	u.cs.c.local.Models = nil
	cache := u.usageCache(p)
	cache.key = "old-account\x00"
	cache.view = codexUsageView{Connected: true, Updated: time.Now(), Limits: []codexUsageRow{{ID: "primary", Known: true, Remaining: 77}}}
	w := apiCall(t, h, "GET", "/api/ui/state", nil)
	var state webui.State
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Connections[1].Limits) != 0 || !state.Connections[1].Updated.IsZero() {
		t.Fatal("new account inherited previous quota")
	}
	cache.mu.Lock()
	w = apiCall(t, h, "GET", "/api/ui/state", nil)
	cache.mu.Unlock()
	_ = json.Unmarshal(w.Body.Bytes(), &state)
	if !state.Connections[1].Refreshing || !state.Connections[1].Updated.IsZero() {
		t.Fatal("in-flight refresh reported fabricated timestamp")
	}
}

func TestUIJSONCodexLoginReplacesStaleCatalogError(t *testing.T) {
	u, h := codexLoginUI(t)
	p, _ := u.cs.get().local.provider("work")
	store, err := codexStoreFor(p)
	if err != nil {
		t.Fatal(err)
	}
	// Model discovery ran before the second account's first login.
	probe := u.probeProvider(p, true)
	if probe.OK || !strings.Contains(probe.Msg, "no such file or directory") {
		t.Fatalf("expected missing credential before login: %+v", probe)
	}
	u.cs.c.local.Catalog.Providers = map[string]providerCatalog{p.Name: {Error: probe.Msg}}
	login := postForm(h, "/settings/codex/login", url.Values{"provider": {p.Name}})
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login: %d", login.Code)
	}
	completeCodexCallback(t, login.Header().Get("Location"))
	if status, problem := waitCodexLogin(t, u); status != "complete" {
		t.Fatalf("login %s: %s", status, problem)
	}
	for _, fromDisk := range []bool{false, true} {
		store.mu.Lock()
		store.loaded = !fromDisk
		store.mu.Unlock()
		var state webui.State
		if err := json.Unmarshal(apiCall(t, h, "GET", "/api/ui/state", nil).Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		for _, connection := range state.Connections {
			if connection.Name == p.Name && (!connection.Connected || connection.Error != "") {
				t.Errorf("disk=%v: completed login still reports an error: %+v", fromDisk, connection)
			}
		}
	}
}

func TestUIJSONCodexRetainsCurrentErrors(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	u, h := codexUI(t)
	p, _ := u.cs.get().local.provider("work")
	store, _ := codexStoreFor(p)
	if err := store.save(usageCredential("work-account")); err != nil {
		t.Fatal(err)
	}
	u.oauthTarget = p
	cache := u.usageCache(p)
	cache.key = accountFromCredential(usageCredential("work-account")).key()
	for _, source := range []string{"oauth", "auth", "usage"} {
		u.oauthError, store.authProblem, cache.view.Error = "", "", ""
		switch source {
		case "oauth":
			u.oauthError = "authorization was denied"
		case "auth":
			store.authProblem = "войдите заново"
		case "usage":
			cache.view.Error = "Codex usage: HTTP 503"
		}
		var state webui.State
		if err := json.Unmarshal(apiCall(t, h, "GET", "/api/ui/state", nil).Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		for _, connection := range state.Connections {
			if connection.Name == p.Name && connection.Error == "" {
				t.Errorf("current %s error was hidden", source)
			}
		}
	}
}

func TestUIJSONRouteProfileGuard(t *testing.T) {
	u, h := testUI(t)
	u.cs.c.local.ActiveProfile = "active"
	w := apiCall(t, h, "POST", "/api/ui/actions", webui.Action{Action: "route.save", Fields: map[string]string{"model": "opus", "scope": "family", "profile": "old", "all": "anthropic"}})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "Активный профиль изменился") {
		t.Fatal("stale profile accepted")
	}
	w = apiCall(t, h, "POST", "/api/ui/actions", webui.Action{Action: "route.save", Fields: map[string]string{"model": "opus", "scope": "family", "profile": "active", "high": "anthropic"}})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "маршруты не изменены") {
		t.Fatal("partial payload could silently disable other efforts")
	}
	if got := (uiBackend{u}).Requests(url.Values{"limit": {"-2"}, "offset": {"-4"}}); got.Limit != 1 || got.Offset != 0 {
		t.Fatalf("bounds: %+v", got)
	}
}
