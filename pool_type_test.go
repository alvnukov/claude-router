package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"localrouter/internal/history"
)

func twoMemberPool(settings map[string]poolSettings) config {
	l := localSetup{
		Providers:    []provider{{Name: "p", BaseURL: "http://p.test/v1"}, {Name: "q", BaseURL: "http://q.test/v1"}},
		Models:       []localModel{{Provider: "p", Model: "a"}, {Provider: "q", Model: "b"}},
		Routes:       map[string]map[string]modelRoute{"local-model": {"default": {Mode: "pool", Pool: "pair"}}},
		ModelPools:   map[string][]poolTarget{"pair": {{Model: "p/a"}, {Model: "q/b"}}},
		PoolSettings: settings,
	}
	return config{local: l, failover: true, balance: 3, firstByte: 5 * time.Second}
}

func TestPoolWithoutTypeIsFailover(t *testing.T) {
	for name, settings := range map[string]map[string]poolSettings{
		"no settings entry":  nil,
		"entry without type": {"pair": {Failover: true, FirstByteSec: 5}},
		"explicit failover":  {"pair": {Type: "failover", Failover: true, FirstByteSec: 5}},
	} {
		cfg := twoMemberPool(settings).forModel("local-model", "")
		hl := newHealth("")
		// The second member looks better on every rating signal and is idle;
		// the first is busy. Failover still starts with the first.
		hl.m["q/b"] = &modelStat{Score: 1, TTFBMs: 10}
		hl.m["p/a"] = &modelStat{Score: 0.3, TTFBMs: 900}
		hl.inflight["p/a"] = 5
		if got := keys(hl.pick(cfg)); got != "p/a,q/b" {
			t.Fatalf("%s: order %s, want pool order", name, got)
		}
	}
}

func TestFailoverPoolCoolingLast(t *testing.T) {
	cfg := twoMemberPool(nil).forModel("local-model", "")
	hl := newHealth("")
	hl.m["p/a"] = &modelStat{Score: 1, CoolUntil: time.Now().Add(time.Minute)}
	hl.m["q/b"] = &modelStat{Score: 1}
	if got := keys(hl.pick(cfg)); got != "q/b,p/a" {
		t.Fatalf("cooling first member: %s", got)
	}
	hl.m["q/b"].CoolUntil = time.Now().Add(30 * time.Second)
	if got := keys(hl.pick(cfg)); got != "q/b,p/a" {
		t.Fatalf("all cooling must order by CoolUntil: %s", got)
	}
	// Both recover, and the second now rates far better: a rating order would
	// put it first, pool order does not.
	hl.m["p/a"] = &modelStat{Score: 0.2, TTFBMs: 900}
	hl.m["q/b"] = &modelStat{Score: 1, TTFBMs: 10}
	if got := keys(hl.pick(cfg)); got != "p/a,q/b" {
		t.Fatalf("recovered first member must lead new sessions: %s", got)
	}
}

func TestPoolTypeUnknownRejected(t *testing.T) {
	l := twoMemberPool(map[string]poolSettings{"pair": {Type: "roundrobin"}}).local
	err := l.validate()
	if err == nil || !strings.Contains(err.Error(), "roundrobin") || !strings.Contains(err.Error(), "pair") {
		t.Fatalf("unknown pool type: %v", err)
	}
}

func TestPoolSaveKeepsType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	cfg := twoMemberPool(map[string]poolSettings{"pair": {Type: poolFailover, FirstByteSec: 5}})
	cfg.upstream, _ = url.Parse("https://api.anthropic.com")
	if err := writeProviders(path, cfg.local); err != nil {
		t.Fatal(err)
	}
	cs := newConfigStore(cfg, path)
	u := newUIServer(history.New(10, ""), cs, newHealth(""))
	// The form has neither a type (until Task 12) nor the numeric balance.
	values := url.Values{"name": {"pair"}, "failover": {"1"}, "first_byte": {"7"}, "probe_every": {"0"}, "max_input_chars": {"0"}}
	r := httptest.NewRequest("POST", "/settings/pool-settings", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	u.settingsPoolSave(w, r)
	html := w.Body.String()
	if !strings.Contains(html, "Настройки пула сохранены") {
		t.Fatalf("save failed: %d %s", w.Code, html)
	}
	if strings.Contains(html, `name="balance"`) {
		t.Fatal("pool form still offers the numeric balance")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"balance":`) {
		t.Fatalf("saved file still has the numeric balance: %s", raw)
	}
	l, err := readProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.PoolSettings["pair"]; got.Type != poolFailover || got.FirstByteSec != 7 {
		t.Fatalf("saved %+v", got)
	}
}

// An old file with the pool's numeric balance still loads; the value is
// ignored, logged once per read, and gone after the next write.
func TestPoolSettingsLegacyBalanceIgnored(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	var s poolSettings
	if err := json.Unmarshal([]byte(`{"failover":true,"first_byte_seconds":5,"balance":3}`), &s); err != nil {
		t.Fatal(err)
	}
	if s != (poolSettings{Failover: true, FirstByteSec: 5}) {
		t.Fatalf("read %+v", s)
	}
	if !strings.Contains(logs.String(), "balance") {
		t.Fatalf("no log line: %q", logs.String())
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "balance") {
		t.Fatalf("written back: %s", out)
	}
}
