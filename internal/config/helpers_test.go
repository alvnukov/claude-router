package config

import (
	"os"
	"testing"
	"time"
)

// startup loads path the way the router does at start: codex ids first, then
// the defaults the router fills from its environment, then the migrations. A
// standby slot only reads.
func startup(path string, standby bool) (Config, error) {
	l, assigned, err := LoadLocal(path)
	if err != nil {
		return Config{}, err
	}
	if assigned && !standby {
		if err := SaveConfigurationMigration(path, l, ".before-codex-ids"); err != nil {
			return Config{}, err
		}
	}
	c := Config{Local: l, Failover: true, FirstByte: 45 * time.Second, Balance: 3, ProbeEvery: 30 * time.Second}
	if standby {
		return c, nil
	}
	return MigrateConfig(c, path)
}

func writeRaw(t *testing.T, path, raw string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// oneProvider is a setup with a single provider "p", preferred first.
func oneProvider(base, preferred string, alts ...string) Local {
	l := Local{Providers: []Provider{{Name: "p", BaseURL: base}}, Preferred: "p/" + preferred}
	for _, m := range append([]string{preferred}, alts...) {
		l.Models = append(l.Models, Model{Provider: "p", Model: m})
	}
	return l
}
