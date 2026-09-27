package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"localrouter/internal/protocolcheck"
)

func TestCommandExitCodes(t *testing.T) {
	for _, tt := range []struct {
		name, source string
		code         int
		status       protocolcheck.Status
	}{
		{"match", "schema v1", 0, protocolcheck.Match},
		{"drift", "schema v2", 1, protocolcheck.Drift},
		{"unknown", "", 2, protocolcheck.Unknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "codex-rs"), 0o700); err != nil {
				t.Fatal(err)
			}
			contract := protocolcheck.Contract{ID: "schema", Path: "codex-rs/schema.rs", Kind: "file"}
			var err error
			contract.SHA256, err = protocolcheck.Fingerprint([]byte("schema v1"), contract)
			if err != nil {
				t.Fatal(err)
			}
			manifest := protocolcheck.Manifest{Version: 1, Repository: "https://github.com/openai/codex", Revision: strings.Repeat("a", 40), ReviewedOn: "2026-09-27", Contracts: []protocolcheck.Contract{contract}}
			data, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			baseline := filepath.Join(dir, "baseline.json")
			if err := os.WriteFile(baseline, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if tt.source != "" {
				if err := os.WriteFile(filepath.Join(dir, "codex-rs/schema.rs"), []byte(tt.source), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, jsonOutput := range []bool{false, true} {
				args := []string{"-codex", dir, "-manifest", baseline}
				if jsonOutput {
					args = append(args, "-json")
				}
				var stdout, stderr bytes.Buffer
				if code := run(args, &stdout, &stderr); code != tt.code {
					t.Fatalf("exit %d, want %d: %s", code, tt.code, stderr.String())
				}
				if jsonOutput {
					var report protocolcheck.Report
					if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || report.Status != tt.status {
						t.Fatalf("report = %s, %v", stdout.String(), err)
					}
				} else if !strings.Contains(stdout.String(), string(tt.status)) {
					t.Fatalf("missing status: %s", stdout.String())
				}
			}
		})
	}
}

func TestCommandRejectsMissingInput(t *testing.T) {
	for _, args := range [][]string{nil, {"-codex", "missing", "-manifest", "missing"}, {"-unknown"}, {"-codex", "missing", "extra"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 || stderr.Len() == 0 {
			t.Fatalf("args %v: exit %d, stderr %q", args, code, stderr.String())
		}
	}
}
