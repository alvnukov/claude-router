package history

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Compatibility of history.jsonl across the move of this code out of package
// main. The code before the move wrote golden/ from home/history.jsonl; the
// moved code has to produce the same bytes, so each side reads what the other
// writes.
//
// A directory holds home/history.jsonl and golden/: testdata/compat is
// synthetic and covers every record shape; ROUTER_COMPAT_DIR names a copy of a
// live ROUTER_HOME, anonymized, kept outside the repository, and is shared
// with the compat test of internal/limits.

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
}

// TestCompatFiles runs this package over each ROUTER_HOME copy and wants the
// outputs the code before the move wrote to golden/, byte for byte:
//
//	go test -run TestCompatFiles -count=1 ./internal/history
//	ROUTER_COMPAT_DIR=<anonymized live copy> go test -run TestCompatFiles -count=1 ./internal/history
//
// It checks that the store
//   - loads history.jsonl as before: records, order, Seq, skipped junk lines,
//     Session derived for records written before sessions were tracked;
//   - appends a record as the same JSON line (field names are the Go names,
//     so a renamed field shows up here) and compacts to the same lines.
func TestCompatFiles(t *testing.T) {
	dirs := []string{filepath.Join("testdata", "compat")}
	if d := os.Getenv("ROUTER_COMPAT_DIR"); d != "" {
		dirs = append(dirs, d)
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			out := compatDump(t, filepath.Join(dir, "home"))
			for _, name := range compatOutputs {
				want := compatRead(t, filepath.Join(dir, "golden", name))
				if got := out[name]; !bytes.Equal(got, want) {
					t.Errorf("%s differs from golden:\n%s", name, compatFirstDiff(got, want))
				}
			}
		})
	}
}

func compatDump(t *testing.T, home string) map[string][]byte {
	out := map[string][]byte{}
	// A copy, so no test writes the fixtures or the live copy.
	hist := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(hist, compatRead(t, filepath.Join(home, "history.jsonl")), 0o600); err != nil {
		t.Fatal(err)
	}

	st := New(compatHistoryMax, hist)
	list, err := json.MarshalIndent(st.List(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	out["history.list.json"] = append(list, '\n')
	var r Record
	if err := json.Unmarshal([]byte(compatAppend), &r); err != nil {
		t.Fatal(err)
	}
	st.Persist(&r)
	out["history.appended.jsonl"] = compatRead(t, hist)
	if err := New(compatCompactMax, hist).CompactAfterDrain(); err != nil {
		t.Fatal(err)
	}
	out["history.compacted.jsonl"] = compatRead(t, hist)
	return out
}

func compatRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
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
