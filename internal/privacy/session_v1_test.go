package privacy

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A session file written before key versions existed keeps its pseudonyms:
// testdata/session_v1 was recorded on the old code, and masking the same text
// in that session must give the recorded body byte for byte and restore it.
func TestSessionV1FixtureStable(t *testing.T) {
	r, err := ParseRules([]byte(`{"entries":[{"kind":"person","forms":["Zorvex"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, r)
	file, err := os.ReadFile(filepath.Join("testdata", "session_v1", "v1fixture.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "session_v1", "masked.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.store.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.store.dir, "v1fixture.jsonl"), file, 0o600); err != nil {
		t.Fatal(err)
	}
	body := requestBody("v1fixture", "hosts 10.8.3.17 10.8.44.201 192.168.5.9 fd00:1234::7 10.8.0.0/16 172.16.4.0/24 02:42:ac:11:00:02")
	masked, req := mustMask(t, e, body)
	if !bytes.Equal(masked, want) {
		t.Fatalf("v1 session pseudonyms changed:\n got %s\nwant %s", masked, want)
	}
	back, err := e.UnmaskJSON(req, masked)
	if err != nil || !bytes.Equal(back, body) {
		t.Fatalf("v1 session roundtrip changed (%v):\n%s\n%s", err, body, back)
	}
	if _, err := os.Stat(filepath.Join(e.store.dir, "v2", "v1fixture.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("v1 session copied to the version 2 path (%v)", err)
	}
	// New mappings of a v1 session are appended at the version 1 path.
	mustMask(t, e, requestBody("v1fixture", "Zorvex"))
	if v := req.Stats().Version; v != prfV1 {
		t.Fatalf("v1 session reported as version %d", v)
	}
	if got := e.Counters().Sessions; len(got) != 1 || got[prfV1] != 1 {
		t.Fatalf("sessions by version = %v, want one v1 session", got)
	}
	after, err := os.ReadFile(filepath.Join(e.store.dir, "v1fixture.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(after, file) || len(after) == len(file) {
		t.Fatalf("v1 session not extended in place:\n%s", after)
	}
}

// A new session lives under sessions/v2, where the pre-version code does not
// look, so a binary rolled back below the fix starts that session afresh
// instead of reading its key with the old PRF.
func TestSessionV2Path(t *testing.T) {
	r, err := ParseRules([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	e := fixedKeyEngine(t, r, fixedKey(1))
	_, req := mustMask(t, e, requestBody("fresh", "net 10.8.0.0/16"))
	mustMask(t, e, requestBody("", "net 10.8.0.0/16"))
	if v := req.Stats().Version; v != prfV2 {
		t.Fatalf("new session reported as version %d", v)
	}
	if got := e.Counters().Sessions; len(got) != 1 || got[prfV2] != 2 {
		t.Fatalf("sessions by version = %v, want the named and the request-only v2 session", got)
	}
	if _, err := os.Stat(filepath.Join(e.store.dir, "fresh.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new session written at the version 1 path (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(e.store.dir, "v2", "fresh.jsonl")); err != nil {
		t.Fatalf("new session missing at the version 2 path: %v", err)
	}
}

// After a rollback and a roll forward one id can exist in both versions. The
// store refuses it rather than choosing a key.
func TestSessionInTwoVersionsRejected(t *testing.T) {
	r, err := ParseRules([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	e := fixedKeyEngine(t, r, fixedKey(1))
	mustMask(t, e, requestBody("twice", "net 10.8.0.0/16"))
	line := []byte(`{"kind":"key","key":"` + strings.Repeat("ab", 32) + `"}` + "\n")
	if err := os.WriteFile(filepath.Join(e.store.dir, "twice.jsonl"), line, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Mask(requestBody("twice", "net 10.8.0.0/16")); err == nil || !strings.Contains(err.Error(), "two versions") {
		t.Fatalf("session in two versions accepted: %v", err)
	}
}
