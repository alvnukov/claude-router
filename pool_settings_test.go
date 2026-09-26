package main

import (
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	conf "localrouter/internal/config"
	"localrouter/internal/history"
)

func TestPoolSettingsDashboardPersistsOnlySelectedPool(t *testing.T) {
	up, _ := url.Parse("https://api.anthropic.com")
	path := filepath.Join(t.TempDir(), "providers.json")
	cs := conf.NewStore(config{Upstream: up, FirstByte: 45 * time.Second, Local: localSetup{ModelPools: map[string][]poolTarget{"a": {}, "b": {}}}}, path)
	u := newUIServer(history.New(10, ""), cs, newHealth(""))
	post := func(values url.Values) string {
		t.Helper()
		r := httptest.NewRequest("POST", "/settings/pool-settings", strings.NewReader(values.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		u.settingsPoolSave(w, r)
		if w.Code != 200 {
			t.Fatalf("status %d", w.Code)
		}
		return w.Body.String()
	}
	values := url.Values{"name": {"a"}, "failover": {"1"}, "first_byte": {"7"}, "probe_every": {"0"}, "max_input_chars": {"20000"}}
	html := post(values)
	if !strings.Contains(html, "Настройки пула сохранены") || strings.Count(html, `class="behavior-form"`) != 2 || strings.Contains(html, `id="behavior"`) {
		t.Fatal("pool forms not rendered independently")
	}
	saved, err := conf.ReadProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	want := poolSettings{Failover: true, FirstByteSec: 7, MaxInputChars: 20000}
	if saved.PoolSettings["a"] != want || saved.PoolSettings["b"].FirstByteSec != 45 {
		t.Fatal("settings leaked to other pool")
	}
	values.Set("first_byte", "-1")
	if !strings.Contains(post(values), "нужно целое число") || cs.Get().Local.PoolSettings["a"] != want {
		t.Fatal("invalid settings were applied")
	}
	values.Set("first_byte", "7")
	values.Set("name", "missing")
	if !strings.Contains(post(values), "не найден") {
		t.Fatal("missing pool accepted")
	}
	// A stale settings form cannot resurrect a deleted pool.
	l := cs.Get().Local.Clone()
	delete(l.ModelPools, "a")
	delete(l.PoolSettings, "a")
	if err := cs.Update(conf.Replace(l, "")); err != nil {
		t.Fatal(err)
	}
	values.Set("name", "a")
	if !strings.Contains(post(values), "не найден") {
		t.Fatal("deleted pool resurrected")
	}
}

func TestPoolsUseIndependentTimeoutAndFailover(t *testing.T) {
	var calls testCalls
	server := fakeEndpoint(t, &calls)
	defer server.Close()
	c := config{Local: oneProvider(server.URL, "slow", "good")}
	c.Local.ModelPools = map[string][]poolTarget{"fast": {{Model: "p/slow"}, {Model: "p/good"}}, "patient": {{Model: "p/slow"}, {Model: "p/good"}}, "stop": {{Model: "p/slow"}, {Model: "p/good"}}}
	c.Local.PoolSettings = map[string]poolSettings{"fast": {Failover: true, FirstByteSec: 1}, "patient": {FirstByteSec: 3}, "stop": {FirstByteSec: 1}}
	c.Local.Routes = map[string]map[string]modelRoute{"local-model": {}}
	for _, tc := range []struct {
		pool, served string
		attempts     int
	}{{"fast", "p/good", 2}, {"patient", "p/slow", 1}, {"stop", "", 1}} {
		t.Run(tc.pool, func(t *testing.T) {
			c.Local.Routes["local-model"]["default"] = modelRoute{Mode: "pool", Pool: tc.pool}
			_, trace := runLocal(t, c.ForModel("local-model", "default"), newHealth(""))
			if trace.Served != tc.served || len(trace.Attempts) != tc.attempts {
				t.Fatalf("served %q attempts %+v", trace.Served, trace.Attempts)
			}
		})
	}
}

func TestPoolProbesRespectMembershipAndIntervals(t *testing.T) {
	c := config{Local: oneProvider("http://example.test/v1", "shared", "only-disabled", "unpooled"), ProbeEvery: time.Second}
	c.Local.Providers = append(c.Local.Providers, provider{Name: "codex", Type: "codex", BaseURL: conf.CodexBaseURL})
	c.Local.Models = append(c.Local.Models, localModel{Provider: "codex", Model: "gpt-test"})
	c.Local.ModelPools = map[string][]poolTarget{"a": {{Model: "p/shared"}, {Model: "codex/gpt-test"}}, "b": {{Model: "p/shared"}}, "off": {{Model: "p/only-disabled"}}}
	c.Local.PoolSettings = map[string]poolSettings{"a": {ProbeSec: 60, FirstByteSec: 40}, "b": {ProbeSec: 10, FirstByteSec: 5}, "off": {}}
	planned := poolProbeConfigs(c)
	if len(planned) != 1 || planned["p/shared"].ProbeEvery != 10*time.Second || planned["p/shared"].FirstByte != 5*time.Second {
		t.Fatal("probe settings not scoped/deduplicated")
	}
	c.Local.PoolSettings["b"] = poolSettings{}
	if got := poolProbeConfigs(c)["p/shared"].ProbeEvery; got != time.Minute {
		t.Fatalf("disabled pool still schedules probes: %v", got)
	}
}
