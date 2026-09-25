//go:build compat || compatgen

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Compatibility of history.jsonl and limits.json across the move of their
// code into internal/history and internal/limits. compat_gen_test.go (tag
// compatgen) runs the code before the move over a ROUTER_HOME copy and writes
// golden/; compat_test.go (tag compat) runs the moved code over the same copy
// and wants every output byte for byte. Equal bytes mean each side reads what
// the other writes.
//
// A directory holds home/ (history.jsonl, limits.json) and golden/:
// testdata/compat is synthetic and covers every record shape; ROUTER_COMPAT_DIR
// names a copy of a live ROUTER_HOME, anonymized, kept outside the repository.

const (
	compatHistoryMax = 300 // ROUTER_UI_HISTORY default
	compatCompactMax = 2

	// compatAppend is a record with every field the local route fills.
	compatAppend = `{"ID":"req_append","Seq":9,"Start":"2026-09-25T11:00:00+03:00","End":"2026-09-25T11:00:03.75+03:00",` +
		`"Path":"/v1/messages","Model":"claude-opus-5-5","Route":"local","Session":"sess-c","Stream":true,` +
		`"Headers":{"User-Agent":"claude-cli/2.1.282"},"ReqBody":"eyJtb2RlbCI6ImEifQ==","OpenAIBody":"eyJtb2RlbCI6ImIifQ==",` +
		`"TrimBefore":4,"TrimAfter":3,"TrimNotes":["note <&>"],"Served":"model-b",` +
		`"Attempts":[{"Model":"model-a","Err":"refused","Dur":250000000},{"Model":"model-b","Err":"","Dur":1000000000}],` +
		`"Status":200,"RespCT":"text/event-stream","RespBytes":"ZXZlbnQ6IHBpbmcKCg==","RespTruncated":false,` +
		`"Resp":{"Blocks":[{"Type":"text","Text":"ok","ID":"","Name":"","Input":""}],"StopReason":"end_turn",` +
		`"Usage":{"output_tokens":2},"Model":"model-b","Error":"","Note":"","Events":2}}`
)

var compatOutputs = []string{
	"history.list.json",       // records as loaded, newest first
	"history.appended.jsonl",  // the file after one more record
	"history.compacted.jsonl", // the file compacted to compatCompactMax lines
	"limits.view.json",        // the Anthropic view while fresh
	"limits.view-stale.json",  // and once stale
	"api-limits.json",         // GET /api/limits body with Codex fixtures
	"limits.saved.json",       // limits.json after one more observation
}

func compatDirs(t *testing.T) []string {
	dirs := []string{filepath.Join("testdata", "compat")}
	if d := os.Getenv("ROUTER_COMPAT_DIR"); d != "" {
		dirs = append(dirs, d)
	}
	return dirs
}

// compatCopy copies home into a temporary directory, so no test writes the
// fixtures or the live copy.
func compatCopy(t *testing.T, home string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"history.jsonl", "limits.json"} {
		data, err := os.ReadFile(filepath.Join(home, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// compatNow is a minute after the newest observation in limits.json, so the
// views do not depend on the wall clock.
func compatNow(t *testing.T, home string) time.Time {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "limits.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		WithHeaders *struct {
			At time.Time `json:"at"`
		} `json:"with_headers"`
		WithoutAt time.Time `json:"without_at"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	at := f.WithoutAt
	if f.WithHeaders != nil && f.WithHeaders.At.After(at) {
		at = f.WithHeaders.At
	}
	if at.IsZero() {
		t.Fatal("limits.json has no observation")
	}
	return at.Add(time.Minute)
}

func compatLimitHeaders() http.Header {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1790341200")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.93")
	h.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed_warning")
	h.Set("Content-Type", "application/json")
	return h
}

// compatCodex covers each Codex source state /api/limits reports.
func compatCodex(now time.Time) []codexUsageView {
	return []codexUsageView{
		{Provider: "codex-a", Connected: true, Plan: "pro", Updated: now.Add(-2 * time.Minute), Attempted: now.Add(-2 * time.Minute),
			Limits: []codexUsageRow{
				{Name: "5h", ID: "primary", Seconds: 18000, Known: true, Remaining: 83, Used: 17, Reset: now.Add(time.Hour)},
				{Name: "weekly", ID: "secondary", Seconds: 604800, Known: true, Remaining: 5, Used: 95, Blocked: true},
			}},
		{Provider: "codex-b", Connected: true, Error: "no response"},
		{Provider: "codex-c", Connected: true, Updated: now.Add(-time.Hour), Limits: []codexUsageRow{{ID: "primary"}}},
		{},
	}
}

func compatJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func compatRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
