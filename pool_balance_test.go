package main

import (
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"localrouter/internal/history"
)

var twoProviders = []provider{{Name: "p", BaseURL: "http://p.test/v1"}, {Name: "q", BaseURL: "http://q.test/v1"}}

// poolsOver builds one pool per entry of types, routed from a model of the
// same name, each with the given members in that order.
func poolsOver(providers []provider, members []string, types map[string]string) localSetup {
	l := localSetup{
		Providers:    providers,
		Routes:       map[string]map[string]modelRoute{},
		ModelPools:   map[string][]poolTarget{},
		PoolSettings: map[string]poolSettings{},
	}
	var targets []poolTarget
	for _, key := range members {
		p, m, _ := strings.Cut(key, "/")
		l.Models = append(l.Models, localModel{Provider: p, Model: m})
		targets = append(targets, poolTarget{Model: key})
	}
	for name, typ := range types {
		l.Routes[name] = map[string]modelRoute{"default": {Mode: "pool", Pool: name}}
		l.ModelPools[name] = targets
		l.PoolSettings[name] = poolSettings{Type: typ, Failover: true, FirstByteSec: 5}
	}
	return l
}

// firstFor is the member a session's request starts on, chosen the way
// handleLocal chooses it.
func firstFor(hl *health, l localSetup, pool, session string) string {
	cfg := config{local: l}.forModel(pool, "")
	return hl.bindCandidates(pool+":"+session, poolRoute{cfg.poolName, cfg.poolType}, hl.pick(cfg))[0].Key
}

func TestBalancePoolSpreadsNewSessions(t *testing.T) {
	l := poolsOver(twoProviders, []string{"p/x", "q/y"}, map[string]string{"bal": poolBalance, "fo": poolFailover})
	hl := newHealth("")
	count := map[string]int{}
	for i := 0; i < 10; i++ {
		count[firstFor(hl, l, "bal", fmt.Sprint("s", i))]++
		if got := firstFor(hl, l, "fo", fmt.Sprint("s", i)); got != "p/x" {
			t.Fatalf("failover pool sent a new session to %s", got)
		}
	}
	if count["p/x"] != 5 || count["q/y"] != 5 {
		t.Fatalf("sequential split %v, want 5/5", count)
	}
	hl, count = newHealth(""), map[string]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := firstFor(hl, l, "bal", fmt.Sprint("c", i))
			mu.Lock()
			count[key]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if count["p/x"] != 5 || count["q/y"] != 5 {
		t.Fatalf("concurrent split %v, want 5/5", count)
	}
}

func TestCodexSingleConnectionKeepsOrder(t *testing.T) {
	codex := []provider{
		{Name: "codex", Type: "codex", BaseURL: codexBaseURL},
		{Name: "work", Type: "codex", BaseURL: codexBaseURL, AuthID: testAuthB},
	}
	hl := newHealth("")
	one := poolsOver(codex, []string{"codex/gpt", "codex/mini"}, map[string]string{"solo": poolBalance})
	for i := 0; i < 4; i++ {
		if got := firstFor(hl, one, "solo", fmt.Sprint("s", i)); got != "codex/gpt" {
			t.Fatalf("one connection: session %d started on %s", i, got)
		}
	}
	two := poolsOver(codex, []string{"codex/gpt", "codex/mini", "work/gpt"}, map[string]string{"bal": poolBalance})
	var got []string
	for i := 0; i < 4; i++ {
		got = append(got, firstFor(hl, two, "bal", fmt.Sprint("s", i)))
	}
	if strings.Join(got, ",") != "codex/gpt,work/gpt,codex/gpt,work/gpt" {
		t.Fatalf("new sessions went to %v; models of one connection must keep pool order", got)
	}
}

func TestBalanceCountsPerPool(t *testing.T) {
	l := poolsOver(twoProviders, []string{"p/x", "q/y"}, map[string]string{"bal": poolBalance, "fo": poolFailover})
	hl := newHealth("")
	for i := 0; i < 3; i++ {
		firstFor(hl, l, "fo", fmt.Sprint("s", i)) // three sessions on p, in another pool
	}
	if a, b := firstFor(hl, l, "bal", "n1"), firstFor(hl, l, "bal", "n2"); a != "p/x" || b != "q/y" {
		t.Fatalf("balance counted other pools: %s,%s", a, b)
	}
}

func TestBalanceSkipsCoolingConnection(t *testing.T) {
	l := poolsOver(twoProviders, []string{"p/x", "q/y"}, map[string]string{"bal": poolBalance})
	hl := newHealth("")
	hl.m["p/x"] = &modelStat{CoolUntil: time.Now().Add(time.Minute)}
	for i := 0; i < 4; i++ {
		if got := firstFor(hl, l, "bal", fmt.Sprint("s", i)); got != "q/y" {
			t.Fatalf("session %d started on cooling %s", i, got)
		}
	}
}

func TestBalanceCriterionIsOneFunction(t *testing.T) {
	l := poolsOver(twoProviders, []string{"p/x", "q/y"}, map[string]string{"bal": poolBalance})
	hl := newHealth("")
	hl.balanceBy = func(h *health, pool string, p provider) int {
		if p.Name == "q" {
			return 0
		}
		return 1
	}
	for i := 0; i < 4; i++ {
		if got := firstFor(hl, l, "bal", fmt.Sprint("s", i)); got != "q/y" {
			t.Fatalf("replaced criterion ignored: %s", got)
		}
	}
}

func TestBalanceBoundSessionStays(t *testing.T) {
	l := poolsOver(twoProviders, []string{"p/x", "q/y"}, map[string]string{"bal": poolBalance})
	hl := newHealth("")
	if a, b := firstFor(hl, l, "bal", "s1"), firstFor(hl, l, "bal", "s2"); a != "p/x" || b != "q/y" {
		t.Fatalf("setup: %s,%s", a, b)
	}
	firstFor(hl, l, "bal", "s3") // p/x: p now has two sessions, q one
	for i := 0; i < 3; i++ {
		if got := firstFor(hl, l, "bal", "s1"); got != "p/x" {
			t.Fatalf("bound session rebalanced to %s", got)
		}
	}
	// s1's connection fails: the session moves to q and stays there,
	// although p now has fewer sessions.
	cfg := config{local: l}.forModel("bal", "")
	for _, c := range hl.pick(cfg) {
		if c.Key == "q/y" {
			hl.moveSession("bal:s1", "p/x", c)
		}
	}
	for i := 0; i < 3; i++ {
		if got := firstFor(hl, l, "bal", "s1"); got != "q/y" {
			t.Fatalf("moved session went back to %s", got)
		}
	}
}

func TestPoolSettingsFormSetsType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	cfg := twoMemberPool(map[string]poolSettings{"pair": {Type: poolFailover, FirstByteSec: 5}})
	cfg.upstream, _ = url.Parse("https://api.anthropic.com")
	if err := writeProviders(path, cfg.local); err != nil {
		t.Fatal(err)
	}
	u := newUIServer(history.New(10, ""), newConfigStore(cfg, path), newHealth(""))
	post := func(typ string) string {
		values := url.Values{"name": {"pair"}, "failover": {"1"}, "first_byte": {"5"}, "probe_every": {"0"}, "max_input_chars": {"0"}}
		if typ != "" {
			values.Set("type", typ)
		}
		r := httptest.NewRequest("POST", "/settings/pool-settings", strings.NewReader(values.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		u.settingsPoolSave(w, r)
		return w.Body.String()
	}
	stored := func() poolSettings {
		l, err := readProviders(path)
		if err != nil {
			t.Fatal(err)
		}
		return l.PoolSettings["pair"]
	}
	html := post(poolBalance)
	if !strings.Contains(html, "Настройки пула сохранены") || stored().Type != poolBalance {
		t.Fatalf("balance not saved: %+v\n%s", stored(), html)
	}
	if !strings.Contains(html, `name="type"`) || strings.Contains(html, `name="balance"`) {
		t.Fatal("pool form must offer the type and not the numeric balance")
	}
	if post(""); stored().Type != poolBalance {
		t.Fatalf("a form without a type reset it: %+v", stored())
	}
	if html := post("zzz"); !strings.Contains(html, `тип пула &#34;zzz&#34; не поддерживается`) || stored().Type != poolBalance {
		t.Fatalf("unknown type: %+v\n%s", stored(), html)
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), `"balance":`) {
		t.Fatalf("numeric balance written: %s", raw)
	}
}
