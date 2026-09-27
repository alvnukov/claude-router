package protocolcheck

import (
	"embed"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
)

//go:embed baseline.json
var testBaseline embed.FS

const fixture = `// unrelated UI
fn draw() {
    draw_window();
}

#[derive(Serialize)]
#[serde(tag = "type")]
pub enum ResponseItem {
    Message { text: String },
    Reasoning { encrypted_content: String },
}

#[cfg(test)]
mod tests {
}
`

func testManifest(t *testing.T, kind, anchor, end string) Manifest {
	t.Helper()
	c := Contract{ID: "response-items", Path: "codex-rs/items.rs", Kind: kind, Anchor: anchor, End: end}
	var err error
	c.SHA256, err = Fingerprint([]byte(fixture), c)
	if err != nil {
		t.Fatal(err)
	}
	return Manifest{Version: 1, Repository: "https://github.com/openai/codex", Revision: strings.Repeat("a", 40), ReviewedOn: "2026-09-27", Contracts: []Contract{c}}
}

func TestCheckDetectsWireChangesWithoutUnrelatedUI(t *testing.T) {
	manifest := testManifest(t, "block", "pub enum ResponseItem {", "")
	for _, tt := range []struct {
		name, source string
		status       Status
	}{
		{"unchanged", fixture, Match},
		{"new field", strings.Replace(fixture, "text: String", "text: String, end_turn: bool", 1), Drift},
		{"new event", strings.Replace(fixture, "    Message", "    Interrupted,\n    Message", 1), Drift},
		{"serialization attribute", strings.Replace(fixture, `tag = "type"`, `tag = "kind"`, 1), Drift},
		{"unrelated UI", strings.Replace(fixture, "draw_window()", "draw_new_window()", 1), Match},
		{"test change", strings.Replace(fixture, "mod tests {", "mod tests {\n    // new test", 1), Match},
		{"CRLF checkout", strings.ReplaceAll(fixture, "\n", "\r\n"), Match},
		{"removed contract", strings.Replace(fixture, "ResponseItem", "ResponseOutput", 1), Unknown},
		{"ambiguous contract", fixture + fixture, Unknown},
		{"unterminated declaration", strings.Split(fixture, "    Message")[0], Unknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := fstest.MapFS{"codex-rs/items.rs": &fstest.MapFile{Data: []byte(tt.source)}}
			report, err := Check(source, manifest)
			if err != nil || report.Status != tt.status || report.Findings[0].Status != tt.status {
				t.Fatalf("Check = %#v, %v; want %s", report, err, tt.status)
			}
		})
	}
}

func TestUnknownDoesNotHideDrift(t *testing.T) {
	manifest := testManifest(t, "block", "pub enum ResponseItem {", "")
	missing := manifest.Contracts[0]
	missing.ID, missing.Path = "missing", "codex-rs/missing.rs"
	manifest.Contracts = append(manifest.Contracts, missing)
	source := fstest.MapFS{"codex-rs/items.rs": &fstest.MapFile{Data: []byte(strings.Replace(fixture, "text: String", "text: Option<String>", 1))}}
	report, err := Check(source, manifest)
	if err != nil || report.Status != Unknown || report.Findings[0].Status != Drift || report.Findings[1].Status != Unknown {
		t.Fatalf("Check = %#v, %v", report, err)
	}
	if report.Findings[1].Error == "" {
		t.Fatal("missing-file diagnosis lost")
	}
}

func TestFocusedFilesAndRanges(t *testing.T) {
	for _, tt := range []struct {
		kind, anchor, end string
		changeTests       Status
	}{
		{"file", "", "", Drift},
		{"prefix", "", "#[cfg(test)]", Match},
		{"range", "pub enum ResponseItem {", "#[cfg(test)]", Match},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			manifest := testManifest(t, tt.kind, tt.anchor, tt.end)
			source := fstest.MapFS{"codex-rs/items.rs": &fstest.MapFile{Data: []byte(strings.Replace(fixture, "mod tests", "mod new_tests", 1))}}
			report, err := Check(source, manifest)
			if err != nil || report.Status != tt.changeTests {
				t.Fatalf("Check = %#v, %v; want %s", report, err, tt.changeTests)
			}
		})
	}
}

func TestInvalidManifestFailsClosed(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"empty contracts", func(m *Manifest) { m.Contracts = nil }},
		{"future version", func(m *Manifest) { m.Version = 2 }},
		{"unpinned revision", func(m *Manifest) { m.Revision = "main" }},
		{"other source", func(m *Manifest) { m.Repository = "https://example.com/codex" }},
		{"bad hash", func(m *Manifest) { m.Contracts[0].SHA256 = "invalid" }},
		{"duplicate ID", func(m *Manifest) { m.Contracts = append(m.Contracts, m.Contracts[0]) }},
		{"path escape", func(m *Manifest) { m.Contracts[0].Path = "codex-rs/../../secret" }},
		{"Windows path escape", func(m *Manifest) { m.Contracts[0].Path = `codex-rs/..\secret` }},
		{"unknown selector", func(m *Manifest) { m.Contracts[0].Kind = "future" }},
		{"missing anchor", func(m *Manifest) { m.Contracts[0].Anchor = "" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			manifest := testManifest(t, "block", "pub enum ResponseItem {", "")
			tt.mutate(&manifest)
			data, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Load(strings.NewReader(string(data))); err == nil {
				t.Fatal("invalid manifest accepted")
			}
			report, err := Check(fstest.MapFS{}, manifest)
			if err == nil || report.Status != Unknown {
				t.Fatalf("invalid manifest yielded %#v, %v", report, err)
			}
		})
	}
}

func TestLoadRejectsIgnoredJSON(t *testing.T) {
	manifest := testManifest(t, "block", "pub enum ResponseItem {", "")
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{
		string(data) + `{}`,
		strings.Replace(string(data), `"version":1`, `"version":1,"contracts_typo":[]`, 1),
		`{`,
	} {
		if _, err := Load(strings.NewReader(source)); err == nil {
			t.Fatalf("invalid JSON accepted: %.80s", source)
		}
	}
}

func TestCommittedManifest(t *testing.T) {
	data, err := testBaseline.ReadFile("baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(strings.NewReader(string(data))); err != nil {
		t.Fatalf("invalid committed baseline: %v", err)
	}
}
