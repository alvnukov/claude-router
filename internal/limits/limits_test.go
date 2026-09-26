package limits

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"localrouter/internal/platform"
)

// The unified-* names below are the ones the Claude Code client reads; every
// value is synthetic. Live traffic has not confirmed either, so nothing below
// claims what Anthropic sends.

func TestAnthropicLimitsKeepsOnlyRatelimitHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Requests-Remaining", "41")
	h["anthropic-ratelimit-example-status"] = []string{"allowed", "second"}
	h.Set("Authorization", "Bearer secret-token")
	h.Set("X-Api-Key", "secret-key")
	h.Set("Set-Cookie", "session=secret")
	h.Set("Request-Id", "req_1")
	h.Set("Retry-After", "5")
	h.Set("Anthropic-Ratelimit-Long", strings.Repeat("x", 1000))

	got := limitHeaders(h)
	want := map[string]string{
		"anthropic-ratelimit-requests-remaining": "41",
		"anthropic-ratelimit-example-status":     "allowed",
		"anthropic-ratelimit-long":               strings.Repeat("x", maxValue),
	}
	if len(got) != len(want) {
		t.Fatalf("kept %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q", k, got[k], v)
		}
	}

	many := http.Header{}
	for i := 0; i < 3*maxHeaders; i++ {
		many.Set(fmt.Sprintf("Anthropic-Ratelimit-N%03d", i), "1")
	}
	kept := limitHeaders(many)
	if len(kept) != maxHeaders || kept["anthropic-ratelimit-n000"] == "" || kept[fmt.Sprintf("anthropic-ratelimit-n%03d", maxHeaders)] != "" {
		t.Fatalf("cap is not the first %d names in order: %d kept", maxHeaders, len(kept))
	}
	long := http.Header{}
	long.Set("Anthropic-Ratelimit-"+strings.Repeat("n", 500), "1")
	if n := len(limitHeaders(long)); n != 0 {
		t.Fatal("unbounded header name kept")
	}
	if n := len(limitHeaders(nil)); n != 0 {
		t.Fatalf("nil header kept %d", n)
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return &buf
}

func TestAnthropicLimitsLogsNamesOnceWithoutValues(t *testing.T) {
	buf := captureLog(t)
	l := New("", MaxAge)
	now := time.Now()
	l.Observe(http.Header{}, now)
	l.Observe(http.Header{}, now.Add(time.Second))
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-B", "value-b-7")
	h.Set("Anthropic-Ratelimit-A", "value-a-9")
	l.Observe(h, now.Add(2*time.Second))
	l.Observe(h, now.Add(3*time.Second))

	out := buf.String()
	if strings.Count(out, "\n") != 2 {
		t.Fatalf("want one line per distinct name set, got:\n%s", out)
	}
	if !strings.Contains(out, "anthropic limits: response headers: none") {
		t.Fatalf("empty set not logged:\n%s", out)
	}
	if !strings.Contains(out, "anthropic limits: response headers: anthropic-ratelimit-a, anthropic-ratelimit-b") {
		t.Fatalf("names not logged sorted:\n%s", out)
	}
	if strings.Contains(out, "value-") {
		t.Fatalf("header values leaked into the log:\n%s", out)
	}
}

func TestAnthropicLimitsHookNeverFails(t *testing.T) {
	captureLog(t)
	var nilStore *Store
	if err := nilStore.ObserveResponse(&http.Response{Header: http.Header{}}); err != nil {
		t.Fatal(err)
	}
	l := New("", MaxAge)
	if err := l.ObserveResponse(nil); err != nil {
		t.Fatal(err)
	}
	if err := l.ObserveResponse(&http.Response{Header: http.Header{}}); err != nil {
		t.Fatal(err)
	}
}

func TestAnthropicLimitsErrorWithoutHeadersDoesNotClaimAbsence(t *testing.T) {
	buf := captureLog(t)
	l := New("", MaxAge)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	for _, status := range []int{http.StatusBadGateway, 529} {
		resp := &http.Response{StatusCode: status, Request: req, Header: http.Header{}}
		if err := l.ObserveResponse(resp); err != nil {
			t.Fatal(err)
		}
		if v := l.View(time.Now()); v.State != "unavailable" {
			t.Fatalf("%d without headers claimed Anthropic sent no limits: %+v", status, v)
		}
	}
	if strings.Contains(buf.String(), "response headers: none") {
		t.Fatalf("error response logged as evidence of absent headers: %s", buf.String())
	}
	if err := l.ObserveResponse(&http.Response{StatusCode: http.StatusNoContent, Request: req, Header: http.Header{}}); err != nil {
		t.Fatal(err)
	}
	if v := l.View(time.Now()); v.State != "no_headers" {
		t.Fatalf("successful response without headers must be diagnostic: %+v", v)
	}
}

// limitKeys is a view's parsed window names and raw header names, sorted:
// what the tests below compare to tell snapshots apart.
func limitKeys(v View) string {
	var keys []string
	for _, w := range v.Windows {
		keys = append(keys, w.Name)
	}
	for k := range v.Raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func limitsHeader(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestAnthropicLimitsViewStates(t *testing.T) {
	captureLog(t)
	now := time.Now()
	with := limitsHeader("Anthropic-Ratelimit-Example-Status", "allowed")

	l := New("", 30*time.Minute)
	if v := l.View(now); v.State != "unavailable" || !v.ObservedAt.IsZero() || v.AgeSeconds != nil || v.MaxAgeSeconds != 1800 || v.Windows != nil || v.Raw != nil {
		t.Fatalf("nothing observed: %+v", v)
	}
	l.Observe(http.Header{}, now.Add(-10*time.Minute))
	if v := l.View(now); v.State != "no_headers" || !v.ObservedAt.Equal(now.Add(-10*time.Minute)) || v.AgeSeconds == nil || *v.AgeSeconds != 600 || v.Windows != nil || v.Raw != nil {
		t.Fatalf("response without headers: %+v", v)
	}
	l.Observe(with, now.Add(-30*time.Minute))
	if v := l.View(now); v.State != "fresh" || *v.AgeSeconds != 1800 || limitKeys(v) != "example" {
		t.Fatalf("headers exactly max age old are still current: %+v", v)
	}
	if v := l.View(now.Add(time.Second)); v.State != "no_headers" {
		t.Fatalf("stale headers must not be shown as current: %+v", v)
	}
	if v := l.View(now.Add(21 * time.Minute)); v.State != "unavailable" || !v.ObservedAt.Equal(now.Add(-10*time.Minute)) || *v.AgeSeconds != 31*60 || v.Windows != nil || v.Raw != nil {
		t.Fatalf("everything stale: %+v", v)
	}

	// A later answer without the headers (an error, say) does not hide a
	// current snapshot.
	l.Observe(with, now.Add(-time.Minute))
	l.Observe(http.Header{}, now)
	if v := l.View(now); v.State != "fresh" || !v.ObservedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("newer response without headers hid current snapshot: %+v", v)
	}
}

func TestAnthropicLimitsIgnoresOlderObservation(t *testing.T) {
	captureLog(t)
	now := time.Now()
	l := New("", MaxAge)
	l.Observe(limitsHeader("Anthropic-Ratelimit-New", "1"), now)
	l.Observe(limitsHeader("Anthropic-Ratelimit-Old", "1"), now.Add(-time.Second))
	if v := l.View(now); limitKeys(v) != "anthropic-ratelimit-new" || !v.ObservedAt.Equal(now) {
		t.Fatalf("older response replaced newer snapshot: %+v", v)
	}
	l2 := New("", MaxAge)
	l2.Observe(http.Header{}, now)
	l2.Observe(http.Header{}, now.Add(-time.Second))
	if v := l2.View(now); !v.ObservedAt.Equal(now) {
		t.Fatalf("older response moved time back: %+v", v)
	}
}

// A slow streaming response may reach the hook minutes after a newer one.
// Its timestamp must not make the newer observation look like an invalid
// future timestamp just because it was used as the clock for the comparison.
func TestAnthropicLimitsSlowOlderResponseDoesNotRegress(t *testing.T) {
	captureLog(t)
	now := time.Now()
	l := New("", MaxAge)
	l.Observe(limitsHeader("Anthropic-Ratelimit-New", "1"), now)
	l.Observe(limitsHeader("Anthropic-Ratelimit-Old", "1"), now.Add(-5*time.Minute))
	if v := l.View(now); !v.ObservedAt.Equal(now) || limitKeys(v) != "anthropic-ratelimit-new" {
		t.Fatalf("slow response replaced newer headers: %+v", v)
	}

	l2 := New("", MaxAge)
	l2.Observe(http.Header{}, now)
	l2.Observe(http.Header{}, now.Add(-5*time.Minute))
	if v := l2.View(now); !v.ObservedAt.Equal(now) {
		t.Fatalf("slow response moved no-headers time back: %+v", v)
	}
}

// A timestamp from the future (clock moved back, hand-edited file) must
// neither look current nor block real observations.
func TestAnthropicLimitsFutureTimestamp(t *testing.T) {
	captureLog(t)
	now := time.Now()
	l := New("", MaxAge)
	l.Observe(limitsHeader("Anthropic-Ratelimit-Future", "1"), now.Add(2*time.Hour))
	if v := l.View(now); v.State != "unavailable" || !v.ObservedAt.IsZero() || v.AgeSeconds != nil {
		t.Fatalf("future snapshot shown: %+v", v)
	}
	l.Observe(limitsHeader("Anthropic-Ratelimit-Now", "1"), now)
	if v := l.View(now); v.State != "fresh" || limitKeys(v) != "anthropic-ratelimit-now" {
		t.Fatalf("future snapshot blocked a real one: %+v", v)
	}
}

func TestAnthropicLimitsPersistRoundTrip(t *testing.T) {
	captureLog(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "limits.json")
	now := time.Now()
	l := New(path, MaxAge)
	t.Cleanup(l.Close)
	l.Observe(limitsHeader("Anthropic-Ratelimit-A", "value-a"), now.Add(-time.Minute))
	l.Observe(http.Header{}, now.Add(-2*time.Minute))
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	requirePrivateFile(t, path)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "limits.json" && e.Name() != "limits.json.lock" {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
	var disk state
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &disk); err != nil || disk.WithHeaders == nil || disk.WithHeaders.Headers["anthropic-ratelimit-a"] != "value-a" || !disk.WithoutAt.Equal(now.Add(-2*time.Minute)) {
		t.Fatalf("file content %s: %v", data, err)
	}

	again := New(path, MaxAge)
	a, _ := json.Marshal(l.View(now))
	b, _ := json.Marshal(again.View(now))
	if string(a) != string(b) {
		t.Fatalf("after restart %s, before %s", b, a)
	}
}

// Two router processes share limits.json during a deploy: a save never
// replaces a newer observation on disk with an older one.
func TestAnthropicLimitsSaveKeepsNewerDiskState(t *testing.T) {
	captureLog(t)
	path := filepath.Join(t.TempDir(), "limits.json")
	now := time.Now()
	older := New(path, MaxAge)
	t.Cleanup(older.Close)
	newer := New(path, MaxAge)
	t.Cleanup(newer.Close)
	older.Observe(limitsHeader("Anthropic-Ratelimit-Old", "1"), now.Add(-5*time.Minute))
	newer.Observe(limitsHeader("Anthropic-Ratelimit-New", "1"), now.Add(-time.Minute))
	newer.Observe(http.Header{}, now)
	if err := newer.Save(); err != nil {
		t.Fatal(err)
	}
	if err := older.Save(); err != nil {
		t.Fatal(err)
	}
	reread := New(path, MaxAge)
	for name, l := range map[string]*Store{"file": reread, "stale process": older} {
		if v := l.View(now); limitKeys(v) != "anthropic-ratelimit-new" {
			t.Fatalf("%s regressed to older snapshot: %+v", name, v)
		}
	}
	var disk state
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &disk); err != nil || !disk.WithoutAt.Equal(now) {
		t.Fatalf("without_at regressed: %s %v", data, err)
	}
}

// holdLock holds the file lock at path, as another router process would.
// The returned func releases it; the end of the test releases it too.
func holdLock(t *testing.T, path string) (release func()) {
	t.Helper()
	held, stop, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- platform.WithLock(context.Background(), path, func() error {
			close(held)
			<-stop
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("hold lock: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			close(stop)
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(release)
	return release
}

// The read-merge-write of one process must not interleave with another's.
func TestAnthropicLimitsSaveWaitsForOtherWriter(t *testing.T) {
	captureLog(t)
	path := filepath.Join(t.TempDir(), "limits.json")
	release := holdLock(t, path+".lock")
	l := New(path, MaxAge)
	t.Cleanup(l.Close)
	l.Observe(limitsHeader("Anthropic-Ratelimit-A", "1"), time.Now())
	saved := make(chan error, 1)
	go func() { saved <- l.Save() }()
	select {
	case err := <-saved:
		t.Fatalf("saved while another writer held the lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("written while another writer held the lock")
	}
	release()
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
}

// A writer that keeps the lock makes a save fail after lockWait, with
// the lock file named, instead of blocking the saver for good.
func TestAnthropicLimitsSaveGivesUpOnHeldLock(t *testing.T) {
	captureLog(t)
	path := filepath.Join(t.TempDir(), "limits.json")
	holdLock(t, path+".lock")
	l := New(path, MaxAge)
	t.Cleanup(l.Close)
	start := time.Now()
	err := l.Save()
	if err == nil || err.Error() != path+".lock: held by another process" {
		t.Fatalf("save under a held lock: %v", err)
	}
	if waited := time.Since(start); waited < lockWait || waited > lockWait+time.Second {
		t.Fatalf("waited %v, want about %v", waited, lockWait)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("written while another writer held the lock")
	}
}

func TestAnthropicLimitsBadFile(t *testing.T) {
	buf := captureLog(t)
	dir := t.TempDir()
	missing := New(filepath.Join(dir, "missing.json"), MaxAge)
	if v := missing.View(time.Now()); v.State != "unavailable" {
		t.Fatalf("missing file: %+v", v)
	}
	missing.Close()
	if _, err := os.Stat(filepath.Join(dir, "missing.json")); !os.IsNotExist(err) {
		t.Fatal("file created without an observation")
	}
	if buf.Len() != 0 {
		t.Fatalf("missing file reported: %q", buf.String())
	}

	path := filepath.Join(dir, "limits.json")
	writeRaw(t, path, "{not json")
	l := New(path, MaxAge)
	t.Cleanup(l.Close)
	if v := l.View(time.Now()); v.State != "unavailable" {
		t.Fatalf("corrupt file: %+v", v)
	}
	if !strings.Contains(buf.String(), "anthropic limits: "+path) {
		t.Fatalf("corrupt file not reported: %q", buf.String())
	}
	l.Observe(limitsHeader("Anthropic-Ratelimit-A", "1"), time.Now())
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	if v := New(path, MaxAge).View(time.Now()); v.State != "fresh" {
		t.Fatalf("corrupt file not replaced: %+v", v)
	}

	// A hand-edited file cannot smuggle other headers or unbounded names in.
	edited := filepath.Join(dir, "edited.json")
	writeRaw(t, edited, `{"with_headers":{"at":"`+time.Now().Format(time.RFC3339Nano)+`","headers":{"authorization":"x","anthropic-ratelimit-ok":"1","anthropic-ratelimit-`+strings.Repeat("n", 500)+`":"1"}}}`)
	if v := New(edited, MaxAge).View(time.Now()); limitKeys(v) != "anthropic-ratelimit-ok" {
		t.Fatalf("file headers not filtered: %+v", v)
	}
}

func TestAnthropicLimitsSaveErrorKeepsMemory(t *testing.T) {
	buf := captureLog(t)
	notDir := filepath.Join(t.TempDir(), "file")
	writeRaw(t, notDir, "")
	l := New(filepath.Join(notDir, "limits.json"), MaxAge)
	t.Cleanup(l.Close)
	l.Observe(limitsHeader("Anthropic-Ratelimit-A", "secret-value"), time.Now())
	if err := l.Save(); err == nil {
		t.Fatal("save into a file path succeeded")
	}
	if v := l.View(time.Now()); v.State != "fresh" {
		t.Fatalf("failed save lost the snapshot: %+v", v)
	}
	l.Observe(limitsHeader("Anthropic-Ratelimit-A", "secret-value"), time.Now().Add(time.Second))
	l.Close()
	out := buf.String()
	if strings.Count(out, "anthropic limits: save") != 1 || strings.Contains(out, "secret-value") {
		t.Fatalf("save failure must be logged once, without values: %q", out)
	}
}

// The proxy never waits for the disk: Observe only schedules a save, and
// Close flushes whatever is pending.
func TestAnthropicLimitsBackgroundSave(t *testing.T) {
	captureLog(t)
	path := filepath.Join(t.TempDir(), "limits.json")
	l := New(path, MaxAge)
	l.Observe(limitsHeader("Anthropic-Ratelimit-A", "1"), time.Now())
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("observation never saved")
		}
		time.Sleep(5 * time.Millisecond)
	}
	l.Observe(limitsHeader("Anthropic-Ratelimit-B", "1"), time.Now().Add(time.Second))
	l.Close()
	l.Close()
	if v := New(path, MaxAge).View(time.Now().Add(time.Second)); limitKeys(v) != "anthropic-ratelimit-b" {
		t.Fatalf("close did not flush the last observation: %+v", v)
	}
	l.Observe(http.Header{}, time.Now().Add(2*time.Second)) // after close: memory only
	var nilStore *Store
	nilStore.Close()
	if err := nilStore.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestAnthropicLimitsConcurrentUse(t *testing.T) {
	captureLog(t)
	l := New(filepath.Join(t.TempDir(), "limits.json"), MaxAge)
	t.Cleanup(l.Close)
	start := time.Now()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				at := start.Add(time.Duration(i*8+g) * time.Millisecond)
				if i%2 == 0 {
					l.Observe(limitsHeader(fmt.Sprintf("Anthropic-Ratelimit-G%d", g), "1"), at)
				} else {
					l.Observe(http.Header{}, at)
				}
				l.View(at)
				if i%50 == 0 {
					_ = l.Save() // racing writers; the view below is what counts
				}
			}
		}(g)
	}
	wg.Wait()
	last := start.Add(time.Duration(199*8+7) * time.Millisecond)
	if v := l.View(last); v.State != "fresh" || !v.ObservedAt.Equal(start.Add(time.Duration(198*8+7)*time.Millisecond)) {
		t.Fatalf("after concurrent use: %+v", v)
	}
}

// Every header is anthropic-ratelimit-<window>-<field>, the field being
// utilization, remaining, limit, reset or status. One function reads them all
// without knowing window names; anything else stays raw under its full name.
func TestParseLimitWindows(t *testing.T) {
	windows, raw := parseWindows(map[string]string{
		"anthropic-ratelimit-unified-5h-utilization":       "0.23",
		"anthropic-ratelimit-unified-5h-reset":             "1790348400",
		"anthropic-ratelimit-unified-7d-utilization":       "1.25",
		"anthropic-ratelimit-unified-7d-status":            "rejected",
		"anthropic-ratelimit-unified-7d_oi-utilization":    "0.1234",
		"anthropic-ratelimit-unified-status":               "allowed_warning",
		"anthropic-ratelimit-requests-limit":               "50",
		"anthropic-ratelimit-requests-remaining":           "41",
		"anthropic-ratelimit-requests-reset":               "2026-09-25T18:00:00+03:00",
		"anthropic-ratelimit-tokens-remaining":             "39999",
		"anthropic-ratelimit-both-utilization":             "0.5",
		"anthropic-ratelimit-both-remaining":               "9",
		"anthropic-ratelimit-both-limit":                   "10",
		"anthropic-ratelimit-over-remaining":               "15",
		"anthropic-ratelimit-over-limit":                   "10",
		"anthropic-ratelimit-zero-remaining":               "0",
		"anthropic-ratelimit-zero-limit":                   "0",
		"anthropic-ratelimit-unified-representative-claim": "five_hour",
		"anthropic-ratelimit-unified-fallback":             "available",
		"anthropic-ratelimit-reset":                        "1790348400",
	})
	got, _ := json.Marshal(windows)
	want := `[` +
		`{"name":"both","remaining_percent":50,"used_percent":50,"remaining":9,"limit":10},` +
		`{"name":"over","remaining_percent":100,"remaining":15,"limit":10},` +
		`{"name":"requests","remaining_percent":82,"remaining":41,"limit":50,"reset_at":"2026-09-25T15:00:00Z"},` +
		`{"name":"tokens","remaining":39999},` +
		`{"name":"unified","status":"allowed_warning"},` +
		`{"name":"unified-5h","remaining_percent":77,"used_percent":23,"reset_at":"2026-09-25T15:00:00Z"},` +
		`{"name":"unified-7d","remaining_percent":0,"used_percent":125,"status":"rejected"},` +
		`{"name":"unified-7d_oi","remaining_percent":87.7,"used_percent":12.3},` +
		`{"name":"zero","remaining":0,"limit":0}` +
		`]`
	if string(got) != want {
		t.Fatalf("windows\n got %s\nwant %s", got, want)
	}
	wantRaw := map[string]string{
		"anthropic-ratelimit-unified-representative-claim": "five_hour",
		"anthropic-ratelimit-unified-fallback":             "available",
		"anthropic-ratelimit-reset":                        "1790348400",
	}
	if !maps.Equal(raw, wantRaw) {
		t.Fatalf("raw %v, want %v", raw, wantRaw)
	}
}

// A value that does not parse stays raw under its full name; nothing is
// guessed from it.
func TestParseLimitWindowsMalformed(t *testing.T) {
	for _, c := range []struct{ name, value string }{
		{"anthropic-ratelimit-unified-5h-utilization", "abc"},
		{"anthropic-ratelimit-unified-5h-utilization", "-0.1"},
		{"anthropic-ratelimit-unified-5h-utilization", "NaN"},
		{"anthropic-ratelimit-unified-5h-utilization", "+Inf"},
		{"anthropic-ratelimit-unified-5h-utilization", "23%"},
		{"anthropic-ratelimit-requests-remaining", "-1"},
		{"anthropic-ratelimit-requests-remaining", "4.5"},
		{"anthropic-ratelimit-requests-limit", "many"},
		{"anthropic-ratelimit-unified-5h-reset", "tomorrow"},
		{"anthropic-ratelimit-unified-5h-reset", "0"},
		{"anthropic-ratelimit-unified-5h-reset", "1790348400000"}, // milliseconds are not guessed
		{"anthropic-ratelimit-unified-5h-status", ""},
		{"anthropic-ratelimit--utilization", "0.5"},
		{"anthropic-ratelimit-utilization", "0.5"},
	} {
		windows, raw := parseWindows(map[string]string{c.name: c.value})
		if v, ok := raw[c.name]; len(windows) != 0 || len(raw) != 1 || !ok || v != c.value {
			t.Fatalf("%s=%q: windows %+v, raw %v", c.name, c.value, windows, raw)
		}
	}

	windows, raw := parseWindows(map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "0.5",
		"anthropic-ratelimit-unified-5h-reset":       "soon",
	})
	if len(windows) != 1 || windows[0].Name != "unified-5h" || !windows[0].ResetAt.IsZero() || raw["anthropic-ratelimit-unified-5h-reset"] != "soon" {
		t.Fatalf("one bad field must not drop the window: %+v %v", windows, raw)
	}

	if windows, raw := parseWindows(nil); len(windows) != 0 || len(raw) != 0 {
		t.Fatalf("no headers: %+v %v", windows, raw)
	}
}

// requirePrivateFile fails unless path is a regular file readable only by its
// owner. NTFS keeps no unix permission bits, so on Windows only the kind is
// checked: the user profile's ACL is what protects the file there.
func requirePrivateFile(t *testing.T, path string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Mode().IsRegular() || runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("%s: mode %v, want a regular file at 0600", path, st.Mode())
	}
}

func writeRaw(t *testing.T, path, raw string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

type gate struct{ atomic.Bool }

func (g *gate) WritesSharedState() bool { return g.Load() }

// Two slots share limits.json during a deploy; the gate says whether this one
// writes it. Until it does, observations stay in memory.
func TestAnthropicLimitsSaveOnlyWhileGateWrites(t *testing.T) {
	captureLog(t)
	path := filepath.Join(t.TempDir(), "limits.json")
	l := New(path, MaxAge)
	t.Cleanup(l.Close)
	var writes gate
	l.SetGate(&writes)
	l.Observe(limitsHeader("Anthropic-Ratelimit-A", "1"), time.Now())
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("written while the gate held writes back: %v", err)
	}
	if v := l.View(time.Now()); v.State != "fresh" {
		t.Fatalf("held-back observation lost: %+v", v)
	}
	writes.Store(true)
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	if v := New(path, MaxAge).View(time.Now()); limitKeys(v) != "anthropic-ratelimit-a" {
		t.Fatalf("gate opened, file not written: %+v", v)
	}
}

// ResetIn is what the settings page prints next to a window's reset time.
func TestAnthropicLimitsViewResetIn(t *testing.T) {
	captureLog(t)
	now := time.Now()
	l := New("", MaxAge)
	l.Observe(limitsHeader(
		"Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(now.Add(90*time.Minute+30*time.Second).Unix(), 10),
		"Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(now.Add(-time.Minute).Unix(), 10),
		"Anthropic-Ratelimit-Requests-Remaining", "3",
	), now)
	got := map[string]string{}
	for _, w := range l.View(now).Windows {
		got[w.Name] = w.ResetIn
	}
	want := map[string]string{"unified-5h": "через 1 ч. 30 мин.", "unified-7d": "ожидается обновление лимита", "requests": ""}
	if !maps.Equal(got, want) {
		t.Fatalf("reset in %v, want %v", got, want)
	}
}

func TestTimeLeft(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "менее минуты"},
		{59*time.Minute + 59*time.Second, "59 мин."},
		{90 * time.Minute, "1 ч. 30 мин."},
		{49 * time.Hour, "2 дн. 1 ч."},
	} {
		if got := TimeLeft(c.d); got != c.want {
			t.Errorf("TimeLeft(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
