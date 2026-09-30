package privacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustMask(t testing.TB, e *Engine, body []byte) ([]byte, *Request) {
	t.Helper()
	out, req, err := e.Mask(body)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(req.Close)
	return out, req
}
func TestFixedPoint(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	a, _ := mustMask(t, e, requestBody("fixed", "Ромашка Петрова admin@romashka.example +7 (000) 123-45-67"))
	b, _ := mustMask(t, e, a)
	if !bytes.Equal(a, b) {
		t.Fatalf("not fixed: %s\n%s", a, b)
	}
}
func TestSessionsUnlinkable(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	a, _ := mustMask(t, e, requestBody("one", "Ромашка 10.1.2.3"))
	b, _ := mustMask(t, e, requestBody("two", "Ромашка 10.1.2.3"))
	ta, _ := lookupString(a, "system")
	tb, _ := lookupString(b, "system")
	if ta == tb {
		t.Fatal("sessions linkable")
	}
	fa, err := os.ReadFile(filepath.Join(e.store.versionDir(prfV2), "one.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	bname := strings.Split(tb, " ")[0]
	if bytes.Contains(fa, []byte(strings.ToLower(bname))) {
		t.Fatal("session contains another session's dictionary")
	}
}
func TestForeignPseudonymRemasked(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	a, _ := mustMask(t, e, requestBody("one", "Ромашка"))
	pseudo, _ := lookupString(a, "system")
	b, req := mustMask(t, e, requestBody("two", pseudo))
	own, _ := lookupString(b, "system")
	if own == pseudo || own == "Ромашка" {
		t.Fatal("foreign pseudonym not remasked")
	}
	back, err := e.UnmaskJSON(req, b)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := lookupString(back, "system"); got != "Ромашка" {
		t.Fatalf("restored foreign alias %q", got)
	}
}
func TestIssueLockAcrossSessions(t *testing.T) {
	home := t.TempDir()
	opt := Options{Home: "/home/testlogin", Hostname: "test-host.local"}
	r := corpusRules(t)
	a, err := Open(home, r, opt)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(home, r, opt)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan string, 40)
	errs := make(chan error, 40)
	for i := range 40 {
		wg.Go(func() {
			engine := a
			if i%2 != 0 {
				engine = b
			}
			body := requestBody(fmt.Sprintf("session-%d", i), "Ромашка")
			out, req, err := engine.Mask(body)
			if err != nil {
				errs <- err
				return
			}
			req.Close()
			value, _ := lookupString(out, "system")
			results <- value
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	seen := map[string]bool{}
	for value := range results {
		if seen[value] {
			t.Fatal("pseudonym collision")
		}
		seen[value] = true
	}
}
func TestPruneSessions(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	for _, id := range []string{"old", "fresh", "live"} {
		_, req := mustMask(t, e, requestBody(id, "Ромашка"))
		if id != "live" {
			req.Close()
		}
	}
	old := time.Now().AddDate(0, 0, -40)
	for _, id := range []string{"old", "live"} {
		if err := os.Chtimes(filepath.Join(e.store.versionDir(prfV2), id+".jsonl"), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := e.Prune(time.Now()); n != 1 || err != nil {
		t.Fatalf("prune: %d %v", n, err)
	}
	t.Run("forget-live", func(t *testing.T) {
		body := requestBody("forget", "Ромашка 10.1.2.3")
		a, req := mustMask(t, e, body)
		if err := e.Forget("forget"); err != nil {
			t.Fatal(err)
		}
		back, err := e.UnmaskJSON(req, a)
		if err != nil || !bytes.Equal(back, body) {
			t.Fatal("forgot live dictionary", err)
		}
		b, _ := mustMask(t, e, body)
		if bytes.Equal(a, b) {
			t.Fatal("forgotten session key reused")
		}
	})
	t.Run("live-request", func(t *testing.T) {
		_, req := mustMask(t, e, requestBody("held", "Ромашка"))
		path := filepath.Join(e.store.versionDir(prfV2), "held.jsonl")
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		if n, err := e.Prune(time.Now()); n != 0 || err != nil {
			t.Fatal("live session pruned", n, err)
		}
		req.Close()
		if n, err := e.Prune(time.Now()); n != 1 || err != nil {
			t.Fatal("released session retained", n, err)
		}
	})
}
func TestResumeWithoutDictionaryRejected(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	body := []byte(`{"metadata":{"user_id":"{\"session_id\":\"resume\"}"},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"opaque","signature":"unchanged"}]}]}`)
	out, req, err := e.Mask(body)
	var rejected *RejectError
	if !errors.As(err, &rejected) || out != nil || req != nil {
		t.Fatal("resume accepted", err)
	}
	mustMask(t, e, requestBody("resume", "Ромашка"))
	masked, _ := mustMask(t, e, body)
	if !bytes.Equal(masked, body) {
		t.Fatal("thinking changed")
	}
}
func TestEntryPseudonymUnique(t *testing.T) {
	for _, pseudo := range []string{"smith", "Ромашка"} {
		raw, _ := json.Marshal(rawRules{Entries: []rawEntry{{Kind: "org", Forms: []string{"Ромашка"}, Pseudonym: pseudo}}})
		r, err := ParseRules(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Open(t.TempDir(), r, Options{}); err == nil {
			t.Fatalf("accepted colliding pseudonym %q", pseudo)
		}
	}
}
func TestPseudonymNeverCollides(t *testing.T) {
	words, err := loadWords()
	if err != nil {
		t.Fatal(err)
	}
	p := &pseudonyms{words: words, reserved: map[string]bool{"кребестов": true}, endings: defaultPolicy.rules.Endings}
	p.candidate = func(_ string, _ []byte, n int) string {
		if n == 0 {
			return "Кребестову"
		}
		return "Жупэфанэвод"
	}
	value, err := p.issue("Иванову", testIPKey)
	if err != nil || value != "Жупэфанэвод" {
		t.Fatal("declensional collision not retried", value, err)
	}
	p.candidate = nil
	seen := make(map[string]bool, 100000)
	for i := range 100000 {
		real := fmt.Sprintf("Subject-%d", i)
		pseudo, err := p.issue(real, testIPKey)
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(pseudo)
		if lower == strings.ToLower(real) || seen[lower] {
			t.Fatal("collision at", i)
		}
		seen[lower] = true
	}
}
func TestSecrets(t *testing.T) {
	r := corpusRules(t)
	e := testEngine(t, r)
	values := []string{"sk-ant-FAKEabcdefghijklmnop1234567890", "sk-FAKEabcdefghijklmnop1234567890", "AKIAIOSFODNN7EXAMPLE", "ghp_FAKEabcdefghijklmnop1234567890", "xoxb-FAKEabcdefghijklmnop1234567890", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJub3QtcmVhbCJ9.RkFLRW9ubHk", "-----BEGIN PRIVATE KEY-----\nFAKE\n-----END PRIVATE KEY-----", "password=FAKE-password-1234", "Authorization: Bearer FAKE-abcdefghijklmnop", "https://login:FAKE-password-5678@service.example"}
	body := requestBody("secrets", strings.Join(values, "\n"))
	a, req := mustMask(t, e, body)
	b, _ := mustMask(t, e, body)
	if !bytes.Equal(a, b) {
		t.Fatal("secret tokens unstable")
	}
	if req.Stats().Masked[KindSecret] != 10 {
		t.Fatal("missed secrets", req.Stats())
	}
	back, err := e.UnmaskJSON(req, a)
	if err != nil || !bytes.Equal(back, body) {
		t.Fatal("secret roundtrip", err)
	}
	disk, err := os.ReadFile(filepath.Join(e.store.versionDir(prfV2), "secrets.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(disk, []byte("FAKE")) || bytes.Contains(disk, []byte("secret:")) {
		t.Fatal("secrets stored on disk")
	}
	r.Allow = append(r.Allow, values[:7]...)
	allowed := testEngine(t, r)
	out, _ := mustMask(t, allowed, requestBody("", strings.Join(values[:7], "\n")))
	text, _ := lookupString(out, "system")
	if text != strings.Join(values[:7], "\n") {
		t.Fatal("secret allow ignored")
	}
}
func TestSecretSameRequest(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	a, req := mustMask(t, e, requestBody("secret-scope", "password=FAKE-password-1234"))
	text, _ := lookupString(a, "system")
	placeholder := strings.TrimPrefix(text, "password=")
	encoded, _ := json.Marshal(placeholder)
	response := []byte(`{"content":[{"type":"tool_use","name":"write","input":{"password":` + string(encoded) + `}}]}`)
	back, err := e.UnmaskJSON(req, response)
	if err != nil || !bytes.Contains(back, []byte("FAKE-password-1234")) {
		t.Fatal("request secret not restored", err)
	}
	_, other := mustMask(t, e, requestBody("secret-scope", "no credential"))
	foreign, err := e.UnmaskJSON(other, response)
	// Strengthened requirement: unresolved credentials in tool input return a
	// correction, never a plausible executable invocation with a fake secret.
	if err == nil || foreign != nil || other.Stats().Unexpected != 1 {
		t.Fatal("foreign secret resolved", err, other.Stats())
	}
}
func TestToolNameCollision(t *testing.T) {
	r, _ := ParseRules([]byte(`{"entries":[{"kind":"project","forms":["Bash"]}]}`))
	e := testEngine(t, r)
	for _, body := range []string{`{"tools":[{"name":"Bash"}]}`, `{"tools":[{"name":"Write","input_schema":{"properties":{"Bash":{"type":"string"}}}}]}`} {
		if err := e.Check([]byte(body)); err == nil {
			t.Fatal("collision accepted")
		}
	}
}
func TestCheckRejects(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	for _, body := range []string{`{"tools":[{"name":"mcp__claude_ai_leak"}]}`, `{"tools":[{"name":"ArtifactWrite"}]}`, `{"system":"100.64.1.2"}`, `{"system":"3fff::1"}`, `{"messages":[{"content":[{"type":"thinking","thinking":"Ромашка"}]}]}`} {
		out, req, err := e.Mask([]byte(body))
		if err == nil || out != nil || req != nil {
			t.Fatalf("unsafe body accepted: %s", body)
		}
	}
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"PRIVATE IMAGE"}}]}]}`)
	out, req := mustMask(t, e, body)
	if bytes.Contains(out, []byte("PRIVATE IMAGE")) || req.Stats().Masked[KindSource] != 1 {
		t.Fatal("image leaked")
	}
	r := corpusRules(t)
	r.Sources = "pass"
	pass := testEngine(t, r)
	kept, _ := mustMask(t, pass, body)
	if !bytes.Equal(kept, body) {
		t.Fatal("source pass ignored")
	}
}
func TestCachePrefixGolden(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	first := requestBody("cache", "Ромашка 10.1.2.3")
	a, _ := mustMask(t, e, first)
	b, _ := mustMask(t, e, requestBody("cache", "Ромашка 10.1.2.3\nnew turn"))
	prefix, _ := lookupString(a, "system")
	next, _ := lookupString(b, "system")
	if next != prefix+"\nnew turn" {
		t.Fatal("prefix changed")
	}
}
func TestSessionPermissionsAndCorruption(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	_, req := mustMask(t, e, requestBody("safe", "Ромашка"))
	req.Close()
	path := filepath.Join(e.store.versionDir(prfV2), "safe.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatal("wrong mode", info.Mode())
	}
	if err := os.WriteFile(path, []byte("{}\ninvalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, req, err := e.Mask(requestBody("safe", "Ромашка")); err == nil || out != nil || req != nil {
		t.Fatal("corrupt dictionary accepted")
	}
}
