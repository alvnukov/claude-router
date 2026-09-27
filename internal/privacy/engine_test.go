package privacy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testEngine(t testing.TB, r *Rules) *Engine {
	t.Helper()
	e, err := Open(t.TempDir(), r, Options{Home: "/home/testlogin", Hostname: "test-host.local"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func requestBody(session, text string) []byte {
	user, _ := json.Marshal(struct {
		Session string `json:"session_id"`
	}{session})
	body, _ := json.Marshal(struct {
		System   string `json:"system"`
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}{System: text, Metadata: struct {
		UserID string `json:"user_id"`
	}{string(user)}})
	return body
}
func TestRoundTrip(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	for _, name := range privacySources(t) {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(privacyCorpus, name))
			if err != nil {
				t.Fatal(err)
			}
			body := requestBody("roundtrip", string(b))
			masked, req, err := e.Mask(body)
			if err != nil {
				t.Fatal(err)
			}
			defer req.Close()
			restored, err := e.UnmaskJSON(req, masked)
			if err != nil || !bytes.Equal(restored, body) {
				t.Fatalf("roundtrip failed (%v):\n%s\n%s", err, body, restored)
			}
		})
	}
}
func TestTwoEngines(t *testing.T) {
	home := t.TempDir()
	r := corpusRules(t)
	opt := Options{Home: "/home/testlogin", Hostname: "test-host.local"}
	e, err := Open(home, r, opt)
	if err != nil {
		t.Fatal(err)
	}
	body := requestBody("stable", "Ромашка 10.1.2.3 admin@romashka.example")
	a, ra, err := e.Mask(body)
	if err != nil {
		t.Fatal(err)
	}
	defer ra.Close()
	second, err := Open(home, r, opt)
	if err != nil {
		t.Fatal(err)
	}
	b, rb, err := second.Mask(body)
	if err != nil {
		t.Fatal(err)
	}
	defer rb.Close()
	if !bytes.Equal(a, b) {
		t.Fatal("engines disagree")
	}
}
func TestNoSessionScope(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	for _, id := range []string{"", "../x", "a/b", strings.Repeat("a", 200), "spaces invalid"} {
		body := requestBody(id, "Ромашка 10.1.2.3")
		out, req, err := e.Mask(body)
		if err != nil {
			t.Fatal(err)
		}
		if req.Stats().Scope != "request" {
			t.Fatal("wrong scope")
		}
		back, err := e.UnmaskJSON(req, out)
		req.Close()
		if err != nil || !bytes.Equal(back, body) {
			t.Fatal("bad transient roundtrip", err)
		}
	}
	if _, err := os.Stat(e.store.dir); !os.IsNotExist(err) {
		t.Fatal("request-only scope created files", err)
	}
}
func TestAutoEntries(t *testing.T) {
	r, _ := ParseRules([]byte(`{}`))
	e := testEngine(t, r)
	body := requestBody("auto", "/home/testlogin/src test-host.local test-host")
	masked, req, err := e.Mask(body)
	if err != nil {
		t.Fatal(err)
	}
	defer req.Close()
	for _, real := range []string{"testlogin", "test-host"} {
		if bytes.Contains(masked, []byte(real)) {
			t.Fatal("auto entry leaked", real)
		}
	}
	back, err := e.UnmaskJSON(req, masked)
	if err != nil || !bytes.Equal(back, body) {
		t.Fatal("auto roundtrip", err)
	}
}
func TestTouchOnUse(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	body := requestBody("touch", "Ромашка")
	_, req, err := e.Mask(body)
	if err != nil {
		t.Fatal(err)
	}
	req.Close()
	path := filepath.Join(e.store.dir, "touch.jsonl")
	old := time.Now().AddDate(0, 0, -40)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	_, req, err = e.Mask(body)
	if err != nil {
		t.Fatal(err)
	}
	req.Close()
	if n, err := e.Prune(time.Now()); err != nil || n != 0 {
		t.Fatalf("pruned touched session: %d %v", n, err)
	}
}
