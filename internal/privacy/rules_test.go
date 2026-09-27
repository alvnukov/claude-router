package privacy

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRulesDefaults(t *testing.T) {
	r, err := ParseRules([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.PublicRange != netip.MustParsePrefix("100.64.0.0/10") {
		t.Errorf("PublicRange = %s", r.PublicRange)
	}
	if r.PublicRange6 != netip.MustParsePrefix("3fff::/20") {
		t.Errorf("PublicRange6 = %s", r.PublicRange6)
	}
	if r.Sources != "withhold" || r.RetainDays != 30 {
		t.Errorf("Sources = %q, RetainDays = %d", r.Sources, r.RetainDays)
	}
}

func TestParseRulesBlocks(t *testing.T) {
	r, err := ParseRules([]byte(`{"networks": ["198.51.100.0/24", "192.0.2.7/25", "2001:db8:4a1::/48"]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []netBlock{
		{Real: netip.MustParsePrefix("198.51.100.0/24"), Pseudo: netip.MustParsePrefix("100.64.0.0/24")},
		{Real: netip.MustParsePrefix("192.0.2.0/25"), Pseudo: netip.MustParsePrefix("100.64.1.0/25")},
		{Real: netip.MustParsePrefix("2001:db8:4a1::/48"), Pseudo: netip.MustParsePrefix("3fff::/48")},
	}
	if len(r.blocks) != len(want) {
		t.Fatalf("blocks = %v", r.blocks)
	}
	for i := range want {
		if r.blocks[i] != want[i] {
			t.Errorf("block %d = %v, want %v", i, r.blocks[i], want[i])
		}
	}
	if r.Networks[1] != netip.MustParsePrefix("192.0.2.0/25") {
		t.Errorf("network kept host bits: %s", r.Networks[1])
	}
}

func TestLoadRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "privacy.json")
	if err := os.WriteFile(path, []byte(`{"domains": ["Romashka.Example"], "retain_days": 7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := LoadRules(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Domains) != 1 || r.Domains[0] != "romashka.example" || r.RetainDays != 7 {
		t.Fatalf("rules = %+v", r)
	}
	if _, err := LoadRules(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("LoadRules accepted a missing file")
	}
}

// TestEngineLoadErrors: every broken rule fails the load, and the error
// names the field or the entry. T8 adds the session file cases.
func TestEngineLoadErrors(t *testing.T) {
	cases := []struct {
		name, rules, want string
	}{
		{"not json", `{"networks": [`, "privacy.json"},
		{"unknown field", `{"retian_days": 3}`, "retian_days"},
		{"bad network", `{"networks": ["10.0.0.0/33"]}`, "networks"},
		{"overlapping networks", `{"networks": ["198.51.100.0/24", "198.51.100.128/25"]}`, "198.51.100.128/25"},
		{"public range overflow", `{"networks": ["198.51.0.0/16"], "public_range": "100.64.0.0/17"}`, "public_range"},
		{"public range full", `{"networks": ["198.51.100.0/24", "203.0.113.0/24"], "public_range": "100.64.0.0/24"}`, "public_range"},
		{"network in pseudonym range", `{"networks": ["3fff:1::/48"]}`, "public_range6"},
		{"network in a masked class", `{"networks": ["10.44.0.0/16"]}`, "10.44.0.0/16"},
		{"public range v6 in v4 field", `{"public_range": "3fff::/20"}`, "public_range"},
		{"public range over a masked class", `{"public_range": "192.168.0.0/16"}`, "public_range"},
		{"unknown entry kind", `{"entries": [{"kind": "city", "forms": ["Pskov"]}]}`, "city"},
		{"entry without forms", `{"entries": [{"kind": "org", "forms": []}]}`, "entries[0]"},
		{"empty form", `{"entries": [{"kind": "org", "forms": ["Romashka", ""]}]}`, "entries[0]"},
		{"star inside a form", `{"entries": [{"kind": "person", "forms": ["Iva*nov"]}]}`, "Iva*nov"},
		{"bad sources", `{"sources": "drop"}`, "sources"},
		{"retain days zero", `{"retain_days": 0}`, "retain_days"},
		{"empty domain", `{"domains": [""]}`, "domains"},
		{"bad allow path", `{"allow_paths": ["messages[*"]}`, "allow_paths"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseRules([]byte(c.rules))
			if err == nil {
				t.Fatal("rules loaded")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not name %q", err, c.want)
			}
		})
	}
}
