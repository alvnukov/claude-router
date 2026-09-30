package privacy

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A session file written before key versions existed keeps its pseudonyms:
// testdata/session_v1 was recorded on the old code, and masking the same text
// in that session must give the recorded body byte for byte and restore it.
func TestSessionV1FixtureStable(t *testing.T) {
	r, err := ParseRules([]byte(`{}`))
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
}
