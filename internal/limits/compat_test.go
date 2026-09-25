package limits

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Compatibility of limits.json across the move of this code out of package
// main. The code before the move wrote golden/ from home/limits.json; the
// moved code has to produce the same bytes, so each side reads what the other
// writes.
//
// A directory holds home/limits.json and golden/: testdata/compat is
// synthetic; ROUTER_COMPAT_DIR names a copy of a live ROUTER_HOME, anonymized,
// kept outside the repository, and is shared with the compat test of
// internal/history and with the /api/limits check of package main.

var compatOutputs = []string{
	"limits.view.json",       // the Anthropic view while fresh
	"limits.view-stale.json", // and once stale
	"limits.saved.json",      // limits.json after one more observation
}

// TestCompatFiles runs this package over each ROUTER_HOME copy and wants the
// outputs the code before the move wrote to golden/, byte for byte:
//
//	go test -run TestCompatFiles -count=1 ./internal/limits
//	ROUTER_COMPAT_DIR=<anonymized live copy> go test -run TestCompatFiles -count=1 ./internal/limits
//
// It checks that the store reads limits.json into the same fresh and stale
// views, and merges and writes it to the same bytes.
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
	now := compatNow(t, home)
	// A copy, so no test writes the fixtures or the live copy.
	path := filepath.Join(t.TempDir(), "limits.json")
	if err := os.WriteFile(path, compatRead(t, filepath.Join(home, "limits.json")), 0o600); err != nil {
		t.Fatal(err)
	}

	l := New(path, MaxAge)
	out["limits.view.json"] = compatJSON(t, l.View(now))
	out["limits.view-stale.json"] = compatJSON(t, l.View(now.Add(MaxAge+time.Minute)))
	l.Observe(compatLimitHeaders(), now)
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	l.Close()
	out["limits.saved.json"] = compatRead(t, path)
	return out
}

// compatNow is a minute after the newest observation in limits.json, so the
// views do not depend on the wall clock.
func compatNow(t *testing.T, home string) time.Time {
	t.Helper()
	var f struct {
		WithHeaders *struct {
			At time.Time `json:"at"`
		} `json:"with_headers"`
		WithoutAt time.Time `json:"without_at"`
	}
	if err := json.Unmarshal(compatRead(t, filepath.Join(home, "limits.json")), &f); err != nil {
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
