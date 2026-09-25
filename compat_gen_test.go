//go:build compatgen

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCompatGenerate writes golden/ with the code before the move:
//
//	go test -tags compatgen -run TestCompatGenerate -count=1 .
//
// Run it at the base commit only; after the move compat_test.go checks the
// moved code against these files and this file is deleted.
func TestCompatGenerate(t *testing.T) {
	for _, dir := range compatDirs(t) {
		out := compatDumpBefore(t, filepath.Join(dir, "home"))
		golden := filepath.Join(dir, "golden")
		if err := os.MkdirAll(golden, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range compatOutputs {
			if err := os.WriteFile(filepath.Join(golden, name), out[name], 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func compatDumpBefore(t *testing.T, home string) map[string][]byte {
	out := map[string][]byte{}
	now := compatNow(t, home)
	dir := compatCopy(t, home)

	hist := filepath.Join(dir, "history.jsonl")
	st := newStore(compatHistoryMax, hist)
	out["history.list.json"] = compatJSON(t, st.list())
	var r record
	if err := json.Unmarshal([]byte(compatAppend), &r); err != nil {
		t.Fatal(err)
	}
	st.persist(&r)
	out["history.appended.jsonl"] = compatRead(t, hist)
	if err := newStore(compatCompactMax, hist).compactAfterDrain(); err != nil {
		t.Fatal(err)
	}
	out["history.compacted.jsonl"] = compatRead(t, hist)

	path := filepath.Join(dir, "limits.json")
	l := newAnthropicLimits(path, anthropicLimitsMaxAge)
	view := l.view(now)
	out["limits.view.json"] = compatJSON(t, view)
	out["limits.view-stale.json"] = compatJSON(t, l.view(now.Add(anthropicLimitsMaxAge+time.Minute)))
	var api bytes.Buffer
	if err := json.NewEncoder(&api).Encode(limitsReportOf(view, compatCodex(now), now)); err != nil {
		t.Fatal(err)
	}
	out["api-limits.json"] = api.Bytes()
	l.observe(compatLimitHeaders(), now)
	if err := l.save(); err != nil {
		t.Fatal(err)
	}
	l.close()
	out["limits.saved.json"] = compatRead(t, path)
	return out
}
