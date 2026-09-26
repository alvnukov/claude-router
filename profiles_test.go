package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	conf "localrouter/internal/config"
	"localrouter/internal/history"
)

func profileFixture(t *testing.T) (*configStore, *health, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.json")
	up, _ := url.Parse("https://api.anthropic.com")
	c := config{Upstream: up, Local: oneProvider("http://example.test/v1", "a", "b"), FirstByte: 45 * time.Second}
	c.Local.FamilyRoutes = map[string]map[string]modelRoute{"opus": {"high": {Mode: "model", Model: "p/a"}}}
	if err := conf.WriteProviders(path, c.Local); err != nil {
		t.Fatal(err)
	}
	cs := conf.NewStore(c, path)
	h := newHealth("")
	cs.OnProfileChange(h.clearSessions)
	return cs, h, path
}

func TestProfilesMigrateOldProvidersWithoutChangingRoute(t *testing.T) {
	cs, _, path := profileFixture(t)
	before := cs.Get().RouteFor("claude-opus-5", "high")
	if err := cs.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	after := cs.Get()
	if after.Local.ActiveProfile != "default" || after.RouteFor("claude-opus-5", "high") != before {
		t.Fatalf("migration changed route: %+v", after.Local)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var disk map[string]json.RawMessage
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	if _, ok := disk["profiles"]; ok {
		t.Fatal("profile definitions stored in global file")
	}
	pointer, err := os.ReadFile(path + ".active-profile")
	if err != nil {
		t.Fatal(err)
	}
	var active string
	if err := json.Unmarshal(pointer, &active); err != nil || active != "default" {
		t.Fatalf("active profile: %q %v", active, err)
	}
	if _, err := os.Stat(path + ".profiles/default.json"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := conf.ReadProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.RouteFor("claude-opus-5", "high") != before {
		t.Fatal("route lost on restart")
	}
}

func TestProfilesActivateChangesNextRouteAndClearsAffinity(t *testing.T) {
	cs, h, _ := profileFixture(t)
	if err := cs.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	if err := cs.ActivateProfile("cloud"); err != nil {
		t.Fatal(err)
	}
	l := cs.Get().Local.Clone()
	l.FamilyRoutes["opus"] = map[string]modelRoute{"high": {Mode: "anthropic"}}
	if err := cs.Update(conf.Replace(l, "")); err != nil {
		t.Fatal(err)
	}
	if err := cs.ActivateProfile("default"); err != nil {
		t.Fatal(err)
	}
	cfg := cs.Get()
	req := anthropicRequest{Model: "claude-opus-5"}
	scope := affinityKey(cfg, []byte(`{"metadata":{"user_id":"{\"session_id\":\"session-1\"}"}}`), req)
	h.bindCandidates(scope, poolRoute{}, []candidate{{Key: "p/a"}})
	if len(h.sessions) == 0 {
		t.Fatal("binding not created")
	}
	if err := cs.ActivateProfile("cloud"); err != nil {
		t.Fatal(err)
	}
	if len(h.sessions) != 0 {
		t.Fatal("old affinity survived activation")
	}
	if got := cs.Get().RouteFor("claude-opus-5", "high"); got.Mode != "anthropic" {
		t.Fatalf("next request uses old route: %+v", got)
	}
	if err := cs.ActivateProfile("missing"); err == nil || cs.Get().Local.ActiveProfile != "cloud" {
		t.Fatal("unknown activation changed active profile")
	}
}

func TestProfilesGlobalProviderAndIndependentEditsSurviveRestart(t *testing.T) {
	cs, _, path := profileFixture(t)
	if err := cs.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateProfile("copy", true); err != nil {
		t.Fatal(err)
	}
	if err := cs.ActivateProfile("copy"); err != nil {
		t.Fatal(err)
	}
	l := cs.Get().Local.Clone()
	l.Providers = append(l.Providers, provider{Name: "shared", BaseURL: "http://shared.test/v1"})
	l.Models = append(l.Models, localModel{Provider: "shared", Model: "new"})
	l.FamilyRoutes["opus"]["high"] = modelRoute{Mode: "model", Model: "shared/new"}
	if err := cs.Update(conf.Replace(l, "")); err != nil {
		t.Fatal(err)
	}
	if err := cs.ActivateProfile("default"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cs.Get().Local.Provider("shared"); !ok {
		t.Fatal("provider not global")
	}
	if got := cs.Get().RouteFor("claude-opus-5", "high"); got.Model != "p/a" {
		t.Fatal("edit leaked between profiles")
	}
	reloaded, err := conf.ReadProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ActiveProfile != "default" || !reloaded.HasModel("shared/new") {
		t.Fatal("global catalog not persisted")
	}
	if err := cs.ActivateProfile("copy"); err != nil {
		t.Fatal(err)
	}
	if got := cs.Get().RouteFor("claude-opus-5", "high"); got.Model != "shared/new" {
		t.Fatal("saved profile edit lost")
	}
}

func TestProfileHTTPActivation(t *testing.T) {
	cs, h, _ := profileFixture(t)
	if err := cs.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateProfile("clean", false); err != nil {
		t.Fatal(err)
	}
	u := newUIServer(history.New(10, ""), cs, h)
	server := u.handler()
	req := httptest.NewRequest("POST", "/api/profiles/clean/activate", strings.NewReader(""))
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	if w.Code != http.StatusOK || cs.Get().Local.ActiveProfile != "clean" {
		t.Fatalf("activation HTTP: %d %s", w.Code, w.Body.String())
	}
	if got := cs.Get().RouteFor("claude-opus-5", "high"); got.Mode != "disabled" {
		t.Fatalf("clean profile not empty: %+v", got)
	}
}

func TestProfilesProviderRemovalRepairsEveryProfile(t *testing.T) {
	cs, _, path := profileFixture(t)
	if err := cs.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateProfile("copy", true); err != nil {
		t.Fatal(err)
	}
	l := cs.Get().Local.Clone()
	l.Providers = nil
	l.Models = nil
	l.FamilyRoutes["opus"]["high"] = modelRoute{Mode: "disabled"}
	if err := cs.Update(conf.Replace(l, "")); err != nil {
		t.Fatal(err)
	}
	if err := cs.ActivateProfile("copy"); err != nil {
		t.Fatal(err)
	}
	if got := cs.Get().RouteFor("claude-opus-5", "high"); got.Mode != "disabled" {
		t.Fatalf("deleted provider still routed in copy: %+v", got)
	}
	if _, err := conf.ReadProviders(path); err != nil {
		t.Fatal(err)
	}
}

func TestProfilesHTTPRejectsCrossOriginAndMissingName(t *testing.T) {
	cs, h, _ := profileFixture(t)
	if err := cs.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	u := newUIServer(history.New(10, ""), cs, h)
	req := httptest.NewRequest("POST", "http://localhost:8788/api/profiles/default/activate", nil)
	req.Header.Set("Origin", "https://attacker.example")
	w := httptest.NewRecorder()
	u.handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status %d", w.Code)
	}
	req = httptest.NewRequest("POST", "http://localhost:8788/api/profiles/unknown/activate", nil)
	w = httptest.NewRecorder()
	u.handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || cs.Get().Local.ActiveProfile != "default" {
		t.Fatalf("unknown profile status %d", w.Code)
	}
}

func TestProfilesCatalogRefreshPreservesBothProfilesOnDisk(t *testing.T) {
	cs, h, path := profileFixture(t)
	if err := cs.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	if err := cs.ActivateProfile("cloud"); err != nil {
		t.Fatal(err)
	}
	l := cs.Get().Local.Clone()
	l.FamilyRoutes["opus"] = map[string]modelRoute{"high": {Mode: "anthropic"}}
	if err := cs.Update(conf.Replace(l, "")); err != nil {
		t.Fatal(err)
	}
	u := newUIServer(history.New(10, ""), cs, h)
	u.fetchAnthropic = func(context.Context) ([]string, error) { return []string{"claude-opus-5"}, nil }
	if err := u.refreshModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	loaded, err := conf.ReadProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RouteFor("claude-opus-5", "high").Mode != "anthropic" || len(loaded.Catalog.Anthropic) != 1 {
		t.Fatal("active profile or catalog lost")
	}
	if err := cs.ActivateProfile("default"); err != nil {
		t.Fatal(err)
	}
	if cs.Get().RouteFor("claude-opus-5", "high").Model != "p/a" {
		t.Fatal("inactive profile lost")
	}
}

func TestProfilesHandEditChangesActiveProfileAndClearsAffinity(t *testing.T) {
	cs, h, path := profileFixture(t)
	if err := cs.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	l := cs.Get().Local.Clone()
	if err := l.UseProfile("cloud"); err != nil {
		t.Fatal(err)
	}
	if err := conf.WriteProviders(path, l); err != nil {
		t.Fatal(err)
	}
	h.bindCandidates("session", poolRoute{}, []candidate{{Key: "p/a"}})
	if err := cs.Reload(); err != nil {
		t.Fatal(err)
	}
	if cs.Get().Local.ActiveProfile != "cloud" || len(h.sessions) != 0 {
		t.Fatal("external activation did not reset session affinity")
	}
}

func TestProfileActivationRoutesNextMessagesRequest(t *testing.T) {
	var cloudCalls, localCalls int
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cloudCalls++; w.WriteHeader(http.StatusAccepted) }))
	defer cloud.Close()
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localCalls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer local.Close()
	cs, h, path := profileFixture(t)
	up, _ := url.Parse(cloud.URL)
	c := cs.Get()
	c.Upstream = up
	cs = conf.NewStore(c, path)
	l := cs.Get().Local.Clone()
	l.Providers[0].BaseURL = local.URL
	if err := cs.Update(conf.Replace(l, "")); err != nil {
		t.Fatal(err)
	}
	if err := cs.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	if err := cs.ActivateProfile("cloud"); err != nil {
		t.Fatal(err)
	}
	l = cs.Get().Local.Clone()
	l.FamilyRoutes["opus"] = map[string]modelRoute{"high": {Mode: "anthropic"}}
	if err := cs.Update(conf.Replace(l, "")); err != nil {
		t.Fatal(err)
	}
	if err := cs.ActivateProfile("default"); err != nil {
		t.Fatal(err)
	}
	st := history.New(10, "")
	u := newUIServer(st, cs, h)
	handler := newMainHandler(cs.Get(), cs, st, h, u)
	body := `{"model":"claude-opus-5","output_config":{"effort":"high"},"messages":[{"role":"user","content":"hi"}]}`
	request := func() int {
		r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if code := request(); code != http.StatusServiceUnavailable || localCalls != 1 || cloudCalls != 0 {
		t.Fatalf("before switch: code %d local %d cloud %d", code, localCalls, cloudCalls)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/api/profiles/cloud/activate", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("activate on API port: %d %s", w.Code, w.Body.String())
	}
	if code := request(); code != http.StatusAccepted || localCalls != 1 || cloudCalls != 1 {
		t.Fatalf("after switch: code %d local %d cloud %d", code, localCalls, cloudCalls)
	}
}

func TestProfilesProviderRenameUpdatesInactiveRoutes(t *testing.T) {
	cs, h, path := profileFixture(t)
	if err := cs.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateProfile("copy", true); err != nil {
		t.Fatal(err)
	}
	u := newUIServer(history.New(10, ""), cs, h)
	values := url.Values{"op": {"update"}, "orig": {"p"}, "name": {"renamed"}, "base_url": {"http://example.test/v1"}}
	req := httptest.NewRequest("POST", "/settings/providers", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	u.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `class="note err"`) {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	if err := cs.ActivateProfile("copy"); err != nil {
		t.Fatal(err)
	}
	if got := cs.Get().RouteFor("claude-opus-5", "high"); got.Model != "renamed/a" {
		t.Fatalf("inactive route not renamed: %+v", got)
	}
	if _, err := conf.ReadProviders(path); err != nil {
		t.Fatal(err)
	}
}

func TestProfilesDeleteSafeguards(t *testing.T) {
	cs, _, _ := profileFixture(t)
	if err := cs.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.DeleteProfile("default"); err == nil {
		t.Fatal("deleted last profile")
	}
	if err := cs.CreateProfile("copy", true); err != nil {
		t.Fatal(err)
	}
	if err := cs.DeleteProfile("default"); err == nil {
		t.Fatal("deleted active profile")
	}
	if err := cs.DeleteProfile("copy"); err != nil {
		t.Fatal(err)
	}
	if len(cs.Get().Local.Profiles) != 1 {
		t.Fatal("deleted wrong profile")
	}
}

func TestProfilesPoolMembersEffortsAndSettingsAreIndependent(t *testing.T) {
	cs, _, path := profileFixture(t)
	l := cs.Get().Local.Clone()
	l.ModelPools = map[string][]poolTarget{"work": {{Model: "p/a", Effort: "high"}, {Model: "p/b", Effort: "low"}}}
	l.PoolSettings = map[string]poolSettings{"work": {Failover: true, FirstByteSec: 7}}
	l.FamilyRoutes["opus"]["high"] = modelRoute{Mode: "pool", Pool: "work"}
	if err := cs.Update(conf.Replace(l, "")); err != nil {
		t.Fatal(err)
	}
	if err := cs.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateProfile("copy", true); err != nil {
		t.Fatal(err)
	}
	if err := cs.ActivateProfile("copy"); err != nil {
		t.Fatal(err)
	}
	l = cs.Get().Local.Clone()
	l.ModelPools["work"] = []poolTarget{{Model: "p/b", Effort: "xhigh"}, {Model: "p/a", Effort: "medium"}}
	if err := cs.Update(conf.Replace(l, "")); err != nil {
		t.Fatal(err)
	}
	if err := cs.SavePoolSettings("work", poolSettings{ProbeSec: 4, FirstByteSec: 11}); err != nil {
		t.Fatal(err)
	}
	if err := cs.ActivateProfile("default"); err != nil {
		t.Fatal(err)
	}
	original := cs.Get().Local
	if original.ModelPools["work"][0] != (poolTarget{Model: "p/a", Effort: "high"}) || original.PoolSettings["work"].FirstByteSec != 7 {
		t.Fatalf("copy changes leaked into default: %+v", original)
	}
	if err := cs.ActivateProfile("copy"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := conf.ReadProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ModelPools["work"][0] != (poolTarget{Model: "p/b", Effort: "xhigh"}) || reloaded.PoolSettings["work"].ProbeSec != 4 {
		t.Fatalf("copy changes lost on disk: %+v", reloaded)
	}
}

func TestProfilesMigrationLeavesOldSettingsBackup(t *testing.T) {
	cs, _, path := profileFixture(t)
	old, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".before-profiles")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != string(old) {
		t.Fatal("backup does not contain old configuration")
	}
	if err := cs.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if again, err := os.ReadFile(path + ".before-profiles"); err != nil || string(again) != string(old) {
		t.Fatal("migration changed backup on second startup")
	}
}

func TestProfilesLoadConfigDoesNotRerunLegacyMigrations(t *testing.T) {
	cs, _, path := profileFixture(t)
	if err := cs.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateProfile("empty", false); err != nil {
		t.Fatal(err)
	}
	if err := cs.ActivateProfile("empty"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROUTER_PROVIDERS_FILE", path)
	loaded := loadConfig()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Local.ActiveProfile != "empty" || len(loaded.Local.Profiles) != 2 || string(before) != string(after) {
		t.Fatalf("startup rewrote profiles: active=%q profiles=%d changed=%t", loaded.Local.ActiveProfile, len(loaded.Local.Profiles), string(before) != string(after))
	}
}

func TestProfileUICloneAndEmpty(t *testing.T) {
	cs, h, _ := profileFixture(t)
	if err := cs.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	u := newUIServer(history.New(10, ""), cs, h)
	post := func(values url.Values) string {
		t.Helper()
		req := httptest.NewRequest("POST", "/settings/profiles", strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		u.handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("profile create: %d %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	if html := post(url.Values{"name": {"clone"}, "mode": {"clone"}}); !strings.Contains(html, "Профиль создан: clone") {
		t.Fatal("clone did not render confirmation")
	}
	if html := post(url.Values{"name": {"empty"}, "mode": {"empty"}}); !strings.Contains(html, "Профиль создан: empty") {
		t.Fatal("empty did not render confirmation")
	}
	if err := cs.ActivateProfile("clone"); err != nil {
		t.Fatal(err)
	}
	if got := cs.Get().RouteFor("claude-opus-5", "high"); got.Model != "p/a" {
		t.Fatalf("clone route: %+v", got)
	}
	if err := cs.ActivateProfile("empty"); err != nil {
		t.Fatal(err)
	}
	if got := cs.Get().RouteFor("claude-opus-5", "high"); got.Mode != "disabled" {
		t.Fatalf("empty route: %+v", got)
	}
}
