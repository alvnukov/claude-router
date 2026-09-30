package privacy

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// After the update a session file of version 1 is not read at all:
// testdata/session_v1 was recorded on the old code. The same session id starts
// afresh with a new key at the version 2 path, the old file stays untouched,
// and the addresses of its history pass back byte for byte.
func TestSessionV1NotRead(t *testing.T) {
	r, err := ParseRules([]byte(`{"entries":[{"kind":"person","forms":["Zorvex"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, r)
	file, err := os.ReadFile(filepath.Join("testdata", "session_v1", "v1fixture.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	history, err := os.ReadFile(filepath.Join("testdata", "session_v1", "masked.json"))
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
	if bytes.Equal(masked, history) {
		t.Fatal("v1 session key was read")
	}
	if v := req.Stats().Version; v != prfV2 {
		t.Fatalf("session reported as version %d", v)
	}
	if got := e.Counters().Sessions; got[prfV1] != 0 || got[prfV2] != 1 {
		t.Fatalf("sessions by version = %v, want no v1 session", got)
	}
	v2, err := os.ReadFile(filepath.Join(e.store.dir, "v2", "v1fixture.jsonl"))
	if err != nil {
		t.Fatalf("session missing at the version 2 path: %v", err)
	}
	if bytes.HasPrefix(v2, bytes.SplitAfter(file, []byte("\n"))[0]) {
		t.Fatal("version 2 session reuses the v1 key")
	}
	if after, err := os.ReadFile(filepath.Join(e.store.dir, "v1fixture.jsonl")); err != nil || !bytes.Equal(after, file) {
		t.Fatalf("v1 file changed (%v)", err)
	}
	back, err := e.UnmaskJSON(req, history)
	if err != nil || !bytes.Equal(back, history) {
		t.Fatalf("v1 history addresses changed (%v):\n%s\n%s", err, history, back)
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
