package main

import (
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPoolSettingsMigrationAndIsolation(t *testing.T) {
	c := config{local: oneProvider("http://example.test/v1", "a"), failover: true, firstByte: 45 * time.Second, balance: 3, probeEvery: 30 * time.Second, maxInputChars: 9000}
	c.local.ModelPools = map[string][]poolTarget{"old": {{Model: "p/a"}}, "zero": {{Model: "p/a"}}}
	c.local.PoolSettings = map[string]poolSettings{"zero": {}}
	c.local.Routes = map[string]map[string]modelRoute{"claude-opus-5": {"high": {Mode: "pool", Pool: "old"}, "low": {Mode: "pool", Pool: "zero"}}}
	migrated, changed := migratePoolSettings(c)
	if !changed || migrated.PoolSettings["old"].FirstByteSec != 45 || migrated.PoolSettings["zero"] != (poolSettings{}) {
		t.Fatal("migration lost existing settings")
	}
	if len(c.local.PoolSettings) != 1 {
		t.Fatal("migration mutated previous snapshot")
	}
	c.local = migrated
	if _, changed := migratePoolSettings(c); changed {
		t.Fatal("migration is not idempotent")
	}
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := writeProviders(path, c.local); err != nil {
		t.Fatal(err)
	}
	var err error
	c.local, err = readProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	c.firstByte, c.probeEvery, c.maxInputChars = time.Second, time.Hour, 1
	old, zero := c.forModel("claude-opus-5", "high"), c.forModel("claude-opus-5", "low")
	if old.firstByte != 45*time.Second || !old.failover || old.probeEvery != 30*time.Second || old.maxInputChars != 9000 {
		t.Fatal("saved pool settings overridden by globals")
	}
	if zero.firstByte != 0 || zero.failover || zero.probeEvery != 0 || zero.maxInputChars != 0 {
		t.Fatal("explicit zeros did not disable settings")
	}
}

func TestPoolSettingsDashboardPersistsOnlySelectedPool(t *testing.T) {
	up, _ := url.Parse("https://api.anthropic.com")
	cs := newConfigStore(config{upstream: up, firstByte: 45 * time.Second, local: localSetup{ModelPools: map[string][]poolTarget{"a": {}, "b": {}}}}, filepath.Join(t.TempDir(), "providers.json"))
	u := newUIServer(newStore(10, ""), cs, newHealth(""))
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
	saved, err := readProviders(cs.provPath)
	if err != nil {
		t.Fatal(err)
	}
	want := poolSettings{Failover: true, FirstByteSec: 7, MaxInputChars: 20000}
	if saved.PoolSettings["a"] != want || saved.PoolSettings["b"].FirstByteSec != 45 {
		t.Fatal("settings leaked to other pool")
	}
	values.Set("first_byte", "-1")
	if !strings.Contains(post(values), "нужно целое число") || cs.get().local.PoolSettings["a"] != want {
		t.Fatal("invalid settings were applied")
	}
	values.Set("first_byte", "7")
	values.Set("name", "missing")
	if !strings.Contains(post(values), "не найден") {
		t.Fatal("missing pool accepted")
	}
	// A stale settings form cannot resurrect a deleted pool.
	l := cs.get().local.clone()
	delete(l.ModelPools, "a")
	delete(l.PoolSettings, "a")
	if err := cs.applyLocal(l, true); err != nil {
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
	c := config{local: oneProvider(server.URL, "slow", "good")}
	c.local.ModelPools = map[string][]poolTarget{"fast": {{Model: "p/slow"}, {Model: "p/good"}}, "patient": {{Model: "p/slow"}, {Model: "p/good"}}, "stop": {{Model: "p/slow"}, {Model: "p/good"}}}
	c.local.PoolSettings = map[string]poolSettings{"fast": {Failover: true, FirstByteSec: 1}, "patient": {FirstByteSec: 3}, "stop": {FirstByteSec: 1}}
	c.local.Routes = map[string]map[string]modelRoute{"local-model": {}}
	for _, tc := range []struct {
		pool, served string
		attempts     int
	}{{"fast", "p/good", 2}, {"patient", "p/slow", 1}, {"stop", "", 1}} {
		t.Run(tc.pool, func(t *testing.T) {
			c.local.Routes["local-model"]["default"] = modelRoute{Mode: "pool", Pool: tc.pool}
			_, trace := runLocal(t, c.forModel("local-model", "default"), newHealth(""))
			if trace.Served != tc.served || len(trace.Attempts) != tc.attempts {
				t.Fatalf("served %q attempts %+v", trace.Served, trace.Attempts)
			}
		})
	}
}

func TestPoolProbesRespectMembershipAndIntervals(t *testing.T) {
	c := config{local: oneProvider("http://example.test/v1", "shared", "only-disabled", "unpooled"), probeEvery: time.Second}
	c.local.Providers = append(c.local.Providers, provider{Name: "codex", Type: "codex", BaseURL: codexBaseURL})
	c.local.Models = append(c.local.Models, localModel{Provider: "codex", Model: "gpt-test"})
	c.local.ModelPools = map[string][]poolTarget{"a": {{Model: "p/shared"}, {Model: "codex/gpt-test"}}, "b": {{Model: "p/shared"}}, "off": {{Model: "p/only-disabled"}}}
	c.local.PoolSettings = map[string]poolSettings{"a": {ProbeSec: 60, FirstByteSec: 40}, "b": {ProbeSec: 10, FirstByteSec: 5}, "off": {}}
	planned := poolProbeConfigs(c)
	if len(planned) != 1 || planned["p/shared"].probeEvery != 10*time.Second || planned["p/shared"].firstByte != 5*time.Second {
		t.Fatal("probe settings not scoped/deduplicated")
	}
	c.local.PoolSettings["b"] = poolSettings{}
	if got := poolProbeConfigs(c)["p/shared"].probeEvery; got != time.Minute {
		t.Fatalf("disabled pool still schedules probes: %v", got)
	}
}
