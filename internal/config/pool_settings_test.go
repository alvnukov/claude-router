package config

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPoolTypeUnknownRejected(t *testing.T) {
	l := Local{
		Providers:    []Provider{{Name: "p", BaseURL: "http://p.test/v1"}, {Name: "q", BaseURL: "http://q.test/v1"}},
		Models:       []Model{{Provider: "p", Model: "a"}, {Provider: "q", Model: "b"}},
		Routes:       map[string]map[string]Route{"local-model": {"default": {Mode: "pool", Pool: "pair"}}},
		ModelPools:   map[string][]PoolTarget{"pair": {{Model: "p/a"}, {Model: "q/b"}}},
		PoolSettings: map[string]PoolSettings{"pair": {Type: "roundrobin"}},
	}
	err := l.Validate()
	if err == nil || !strings.Contains(err.Error(), "roundrobin") || !strings.Contains(err.Error(), "pair") {
		t.Fatalf("unknown pool type: %v", err)
	}
}

// An old file with the pool's numeric balance still loads; the value is
// ignored, logged once per read, and gone after the next write.
func TestPoolSettingsLegacyBalanceIgnored(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	var s PoolSettings
	if err := json.Unmarshal([]byte(`{"failover":true,"first_byte_seconds":5,"balance":3}`), &s); err != nil {
		t.Fatal(err)
	}
	if s != (PoolSettings{Failover: true, FirstByteSec: 5}) {
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

func TestPoolSettingsMigrationAndIsolation(t *testing.T) {
	c := Config{Local: oneProvider("http://example.test/v1", "a"), Failover: true, FirstByte: 45 * time.Second, Balance: 3, ProbeEvery: 30 * time.Second, MaxInputChars: 9000}
	c.Local.ModelPools = map[string][]PoolTarget{"old": {{Model: "p/a"}}, "zero": {{Model: "p/a"}}}
	c.Local.PoolSettings = map[string]PoolSettings{"zero": {}}
	c.Local.Routes = map[string]map[string]Route{"claude-opus-5": {"high": {Mode: "pool", Pool: "old"}, "low": {Mode: "pool", Pool: "zero"}}}
	migrated, changed := migratePoolSettings(c)
	if !changed || migrated.PoolSettings["old"].FirstByteSec != 45 || migrated.PoolSettings["zero"] != (PoolSettings{}) {
		t.Fatal("migration lost existing settings")
	}
	if len(c.Local.PoolSettings) != 1 {
		t.Fatal("migration mutated previous snapshot")
	}
	c.Local = migrated
	if _, changed := migratePoolSettings(c); changed {
		t.Fatal("migration is not idempotent")
	}
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := WriteProviders(path, c.Local); err != nil {
		t.Fatal(err)
	}
	var err error
	c.Local, err = ReadProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	c.FirstByte, c.ProbeEvery, c.MaxInputChars = time.Second, time.Hour, 1
	old, zero := c.ForModel("claude-opus-5", "high"), c.ForModel("claude-opus-5", "low")
	if old.FirstByte != 45*time.Second || !old.Failover || old.ProbeEvery != 30*time.Second || old.MaxInputChars != 9000 {
		t.Fatal("saved pool settings overridden by globals")
	}
	if zero.FirstByte != 0 || zero.Failover || zero.ProbeEvery != 0 || zero.MaxInputChars != 0 {
		t.Fatal("explicit zeros did not disable settings")
	}
}
