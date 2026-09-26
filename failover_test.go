package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"localrouter/internal/history"
)

type testCalls struct {
	mu     sync.Mutex
	values []string
}

func (c *testCalls) add(model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values = append(c.values, model)
}
func (c *testCalls) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.values...)
}
func (c *testCalls) reset() { c.mu.Lock(); defer c.mu.Unlock(); c.values = nil }

// fakeEndpoint answers /chat/completions per model: "good" -> 200, "bad" -> 500,
// "slow" -> sleeps, "gone" -> 404.
func fakeEndpoint(t *testing.T, calls *testCalls) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req) // a bad body routes as model ""
		calls.add(req.Model)
		switch req.Model {
		case "bad":
			http.Error(w, `{"error":"boom"}`, 500)
		case "gone":
			http.Error(w, `{"error":"model not found"}`, 404)
		case "slow":
			time.Sleep(1500 * time.Millisecond)
			fallthrough
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok from `+req.Model+`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		}
	}))
}

func runLocal(t *testing.T, cfg config, hl *health) (*httptest.ResponseRecorder, *history.Trace) {
	t.Helper()
	body := []byte(`{"model":"local-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	tr := &history.Trace{}
	handleLocal(w, r, cfg, body, tr, hl)
	return w, tr
}

func TestFailoverToNextModel(t *testing.T) {
	var calls testCalls
	srv := fakeEndpoint(t, &calls)
	defer srv.Close()
	cfg := config{Local: oneProvider(srv.URL, "bad", "gone", "good"), Failover: true, FirstByte: 5 * time.Second}
	hl := newHealth("")
	w, tr := runLocal(t, cfg, hl)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "ok from good") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if tr.Served != "p/good" || len(tr.Attempts) != 3 || strings.Join(calls.snapshot(), ",") != "bad,gone,good" {
		t.Fatalf("served=%s attempts=%+v calls=%v", tr.Served, tr.Attempts, calls.snapshot())
	}
	if s := hl.snapshot("p/bad"); s.Fail != 1 || !s.Cooling() || s.Score >= 1 {
		t.Fatalf("bad stat %+v", s)
	}
	if s := hl.snapshot("p/good"); s.OK != 1 || s.ServedAfter != 1 {
		t.Fatalf("good stat %+v", s)
	}
	// Next request: bad is cooling, so good goes first.
	calls.reset()
	_, tr = runLocal(t, cfg, hl)
	if tr.Served != "p/good" || len(tr.Attempts) != 1 {
		t.Fatalf("second: served=%s attempts=%+v", tr.Served, tr.Attempts)
	}
}

func TestFirstByteTimeoutFailsOver(t *testing.T) {
	var calls testCalls
	srv := fakeEndpoint(t, &calls)
	defer srv.Close()
	cfg := config{Local: oneProvider(srv.URL, "slow", "good"), Failover: true, FirstByte: 200 * time.Millisecond}
	hl := newHealth("")
	w, tr := runLocal(t, cfg, hl)
	if w.Code != 200 || tr.Served != "p/good" {
		t.Fatalf("code=%d served=%s attempts=%+v", w.Code, tr.Served, tr.Attempts)
	}
	if !strings.Contains(tr.Attempts[0].Err, "no response within") {
		t.Fatalf("attempt err: %q", tr.Attempts[0].Err)
	}
}

func TestFailoverOffReportsError(t *testing.T) {
	var calls testCalls
	srv := fakeEndpoint(t, &calls)
	defer srv.Close()
	cfg := config{Local: oneProvider(srv.URL, "bad", "good"), Failover: false, FirstByte: time.Second}
	w, tr := runLocal(t, cfg, newHealth(""))
	if w.Code != 500 || len(tr.Attempts) != 1 || len(calls.snapshot()) != 1 {
		t.Fatalf("code=%d attempts=%+v calls=%v", w.Code, tr.Attempts, calls.snapshot())
	}
}

func TestPickOrder(t *testing.T) {
	hl := newHealth("")
	cfg := config{Local: oneProvider("http://h/v1", "a", "b", "c"), Failover: true}
	names := func(cs []candidate) string {
		var s []string
		for _, c := range cs {
			s = append(s, c.Key)
		}
		return strings.Join(s, ",")
	}
	if got := names(hl.pick(cfg)); got != "p/a,p/b,p/c" {
		t.Fatalf("fresh: %s", got)
	}
	hl.record("p/a", false, 0, "x") // a cools down
	hl.record("p/c", true, 100*time.Millisecond, "")
	hl.record("p/b", true, 500*time.Millisecond, "")
	if got := names(hl.pick(cfg)); got != "p/c,p/b,p/a" {
		t.Fatalf("after failure: %s", got)
	}
	for i := 0; i < 3; i++ {
		hl.record("p/a", true, 50*time.Millisecond, "")
	}
	if got := names(hl.pick(cfg)); got != "p/a,p/c,p/b" { // healthy again: preferred leads
		t.Fatalf("recovered: %s", got)
	}
	cfg.Failover = false
	if got := names(hl.pick(cfg)); got != "p/a" {
		t.Fatalf("failover off: %s", got)
	}
}

// oneProvider is a setup with a single provider "p", preferred first.
func oneProvider(base, preferred string, alts ...string) localSetup {
	l := localSetup{Providers: []provider{{Name: "p", BaseURL: base}}, Preferred: "p/" + preferred}
	for _, m := range append([]string{preferred}, alts...) {
		l.Models = append(l.Models, localModel{Provider: "p", Model: m})
	}
	return l
}
