//go:build compat

package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"localrouter/internal/history"
	"localrouter/internal/limits"
)

// TestCompatFiles runs the code moved into internal/history and
// internal/limits over each ROUTER_HOME copy and wants the outputs the code
// before the move wrote to golden/, byte for byte:
//
//	go test -tags compat -run TestCompatFiles -count=1 .
//	ROUTER_COMPAT_DIR=<anonymized live copy> go test -tags compat -run TestCompatFiles -count=1 .
//
// It checks that the moved code
//   - loads history.jsonl as before: records, order, Seq, skipped junk lines,
//     Session derived for records written before sessions were tracked;
//   - appends a record as the same JSON line (field names are the Go names,
//     so a renamed field shows up here) and compacts to the same lines;
//   - reads limits.json into the same fresh and stale views;
//   - builds the same GET /api/limits body next to Codex sources;
//   - merges and writes limits.json to the same bytes.
func TestCompatFiles(t *testing.T) {
	for _, dir := range compatDirs(t) {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			out := compatDumpAfter(t, filepath.Join(dir, "home"))
			for _, name := range compatOutputs {
				want := compatRead(t, filepath.Join(dir, "golden", name))
				if got := out[name]; !bytes.Equal(got, want) {
					t.Errorf("%s differs from golden:\n%s", name, compatFirstDiff(got, want))
				}
			}
		})
	}
}

func compatDumpAfter(t *testing.T, home string) map[string][]byte {
	out := map[string][]byte{}
	now := compatNow(t, home)
	dir := compatCopy(t, home)

	hist := filepath.Join(dir, "history.jsonl")
	st := history.New(compatHistoryMax, hist)
	out["history.list.json"] = compatJSON(t, st.List())
	var r history.Record
	if err := json.Unmarshal([]byte(compatAppend), &r); err != nil {
		t.Fatal(err)
	}
	st.Persist(&r)
	out["history.appended.jsonl"] = compatRead(t, hist)
	if err := history.New(compatCompactMax, hist).CompactAfterDrain(); err != nil {
		t.Fatal(err)
	}
	out["history.compacted.jsonl"] = compatRead(t, hist)

	path := filepath.Join(dir, "limits.json")
	l := limits.New(path, limits.MaxAge)
	view := l.View(now)
	out["limits.view.json"] = compatJSON(t, view)
	out["limits.view-stale.json"] = compatJSON(t, l.View(now.Add(limits.MaxAge+time.Minute)))
	var api bytes.Buffer
	if err := json.NewEncoder(&api).Encode(limitsReportOf(view, compatCodex(now), now)); err != nil {
		t.Fatal(err)
	}
	out["api-limits.json"] = api.Bytes()
	l.Observe(compatLimitHeaders(), now)
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	l.Close()
	out["limits.saved.json"] = compatRead(t, path)
	return out
}

// compatFirstDiff shows the first line that differs, so a failure on a large
// live copy stays readable.
func compatFirstDiff(got, want []byte) string {
	g, w := bytes.Split(got, []byte("\n")), bytes.Split(want, []byte("\n"))
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl []byte
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if !bytes.Equal(gl, wl) {
			return "line " + strconv.Itoa(i+1) + "\n got: " + compatClip(gl) + "\nwant: " + compatClip(wl)
		}
	}
	return "equal lines, different bytes"
}

func compatClip(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "..."
	}
	return string(b)
}
