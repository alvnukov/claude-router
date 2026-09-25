package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// twoCodexPool routes local-model to a failover pool [codex/gpt, work/gpt].
// The config goes through forModel exactly as the request path does.
func twoCodexPool() config {
	l := localSetup{
		Providers: []provider{
			{Name: "codex", Type: "codex", BaseURL: codexBaseURL},
			{Name: "work", Type: "codex", BaseURL: codexBaseURL, AuthID: testAuthB},
		},
		Models:     []localModel{{Provider: "codex", Model: "gpt"}, {Provider: "work", Model: "gpt"}},
		Routes:     map[string]map[string]modelRoute{"local-model": {"default": {Mode: "pool", Pool: "gpt"}}},
		ModelPools: map[string][]poolTarget{"gpt": {{Model: "codex/gpt"}, {Model: "work/gpt"}}},
	}
	c := config{local: l, failover: true, balance: 3, firstByte: 5 * time.Second}
	return c.forModel("local-model", "")
}

const codexStreamOK = `data: {"type":"response.output_text.delta","delta":"ok"}

` +
	`data: {"type":"response.completed","response":{"status":"completed"}}

`

func codexStatusByAccount(t *testing.T, status map[string]int) *sync.Map {
	t.Helper()
	calls := &sync.Map{}
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "chatgpt.com" {
			return usageResponse(400, `{"error":"invalid_grant"}`), nil
		}
		acct := r.Header.Get("ChatGPT-Account-Id")
		n, _ := calls.LoadOrStore(acct, new(int))
		*n.(*int)++
		if code := status[acct]; code != 0 && code != 200 {
			return usageResponse(code, `{"error":{"message":"limit"}}`), nil
		}
		return usageResponse(200, codexStreamOK), nil
	})
	return calls
}

func seedTwoConnections(t *testing.T) {
	useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
	seedConnection(t, provider{Name: "codex", Type: "codex"}, "acct-a")
	seedConnection(t, provider{Name: "work", Type: "codex", AuthID: testAuthB}, "acct-b")
}

// runLocalRequest is runLocal with a Claude Code session id, so affinity
// binds, and the client's context, so a test can go away mid-answer.
func runLocalRequest(t *testing.T, ctx context.Context, cfg config, hl *health, session string, stream bool) (*httptest.ResponseRecorder, *localTrace) {
	t.Helper()
	uid, _ := json.Marshal(map[string]string{"session_id": session})
	body, _ := json.Marshal(map[string]any{
		"model":      "local-model",
		"max_tokens": 10,
		"stream":     stream,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"metadata":   map[string]string{"user_id": string(uid)},
	})
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(body))).WithContext(ctx)
	w := httptest.NewRecorder()
	tr := &localTrace{}
	handleLocal(w, r, cfg, body, tr, hl)
	return w, tr
}

func runLocalSession(t *testing.T, cfg config, hl *health, session string) (*httptest.ResponseRecorder, *localTrace) {
	t.Helper()
	return runLocalRequest(t, context.Background(), cfg, hl, session, false)
}

func TestCodexFailoverNewSessionSkipsLimitedFirst(t *testing.T) {
	seedTwoConnections(t)
	cfg, hl := twoCodexPool(), newHealth("")
	status := map[string]int{"acct-a": 429}
	codexStatusByAccount(t, status)
	w, tr := runLocalSession(t, cfg, hl, "s1")
	if w.Code != 200 || tr.Served != "work/gpt" {
		t.Fatalf("429 on first: %d served %s", w.Code, tr.Served)
	}
	// The first connection is cooling now: the next new session goes straight
	// to the second without spending a request on the first.
	_, tr = runLocalSession(t, cfg, hl, "s2")
	if tr.Served != "work/gpt" || len(tr.Attempts) != 1 {
		t.Fatalf("new session during cooldown: %s after %d attempts", tr.Served, len(tr.Attempts))
	}
}

func TestCodexSessionStaysOnConnection(t *testing.T) {
	seedTwoConnections(t)
	cfg, hl := twoCodexPool(), newHealth("")
	status := map[string]int{}
	codexStatusByAccount(t, status)
	for i := 0; i < 3; i++ {
		if _, tr := runLocalSession(t, cfg, hl, "s1"); tr.Served != "codex/gpt" {
			t.Fatalf("healthy first member not used: %s", tr.Served)
		}
	}
	// Load on the first member moves neither a bound session nor a new one.
	hl.mu.Lock()
	hl.inflight["codex/gpt"] = 9
	hl.mu.Unlock()
	for _, s := range []string{"s1", "busy"} {
		if _, tr := runLocalSession(t, cfg, hl, s); tr.Served != "codex/gpt" {
			t.Fatalf("%s: busy but healthy member skipped: %s", s, tr.Served)
		}
	}
	hl.mu.Lock()
	hl.inflight["codex/gpt"] = 0
	hl.mu.Unlock()
	status["acct-a"] = 429
	if _, tr := runLocalSession(t, cfg, hl, "s1"); tr.Served != "work/gpt" {
		t.Fatalf("failure did not move the session: %s", tr.Served)
	}
	// The first connection recovers completely; s1 stays where it moved.
	delete(status, "acct-a")
	hl.mu.Lock()
	hl.m["codex/gpt"].CoolUntil = time.Time{}
	hl.m["codex/gpt"].Score = 0.2
	hl.m["codex/gpt"].TTFBMs = 900
	hl.mu.Unlock()
	for i := 0; i < 3; i++ {
		if _, tr := runLocalSession(t, cfg, hl, "s1"); tr.Served != "work/gpt" {
			t.Fatalf("session returned to the recovered member: %s", tr.Served)
		}
	}
	// Only new sessions go back to the first connection.
	if _, tr := runLocalSession(t, cfg, hl, "fresh"); tr.Served != "codex/gpt" {
		t.Fatalf("new session after recovery went to %s", tr.Served)
	}
}

func TestCodexAll429KeepsBehavior(t *testing.T) {
	seedTwoConnections(t)
	cfg, hl := twoCodexPool(), newHealth("")
	codexStatusByAccount(t, map[string]int{"acct-a": 429, "acct-b": 429})
	w, tr := runLocalSession(t, cfg, hl, "s1")
	if w.Code != 429 {
		t.Fatalf("client got %d, want 429", w.Code)
	}
	a, b := hl.snapshot("codex/gpt"), hl.snapshot("work/gpt")
	if !a.Cooling() || !b.Cooling() {
		t.Fatal("both members must cool down")
	}
	if d := time.Until(a.CoolUntil); d <= 0 || d > cooldownBase {
		t.Fatalf("first failure cooldown %s, want <= %s", d, cooldownBase)
	}
	order := hl.pick(cfg)
	if order[1].Stat.CoolUntil.Before(order[0].Stat.CoolUntil) {
		t.Fatal("cooling members not ordered by CoolUntil")
	}
	hl.mu.Lock()
	var pinned string
	for _, b := range hl.sessions {
		pinned = b.Model
	}
	hl.mu.Unlock()
	if last := tr.Attempts[len(tr.Attempts)-1].Model; pinned != last {
		t.Fatalf("session pinned to %s, last tried %s", pinned, last)
	}
}

// cancelAfterFirstEvent sends one delta, then acts as the client pressing Esc:
// it cancels the client's context and fails the read once the cancel lands.
type cancelAfterFirstEvent struct {
	ctx    context.Context
	cancel context.CancelFunc
	sent   bool
}

func (b *cancelAfterFirstEvent) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, `data: {"type":"response.output_text.delta","delta":"ok"}`+"\n\n"), nil
	}
	b.cancel()
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *cancelAfterFirstEvent) Close() error { return nil }

func TestClientCancelKeepsBinding(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			seedTwoConnections(t)
			cfg, hl := twoCodexPool(), newHealth("")
			calls := &sync.Map{}
			var cancelNext context.CancelFunc
			old := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = old })
			http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
				n, _ := calls.LoadOrStore(r.Header.Get("ChatGPT-Account-Id"), new(int))
				*n.(*int)++
				if cancelNext != nil {
					resp := usageResponse(200, "")
					resp.Body = &cancelAfterFirstEvent{ctx: r.Context(), cancel: cancelNext}
					cancelNext = nil
					return resp, nil
				}
				return usageResponse(200, codexStreamOK), nil
			})
			if _, tr := runLocalRequest(t, context.Background(), cfg, hl, "s1", stream); tr.Served != "codex/gpt" {
				t.Fatalf("first request served by %q", tr.Served)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cancelNext = cancel
			runLocalRequest(t, ctx, cfg, hl, "s1", stream)

			if st := hl.snapshot("codex/gpt"); st.Fail != 0 || st.Cooling() {
				t.Fatalf("client cancel counted as a failure: %+v", st)
			}
			if n := hl.load("codex/gpt"); n != 0 {
				t.Fatalf("in-flight count leaked: %d", n)
			}
			if _, ok := calls.Load("acct-b"); ok {
				t.Fatal("cancelled request was retried on the other connection")
			}
			_, tr := runLocalSession(t, cfg, hl, "s1")
			if tr.Served != "codex/gpt" || len(tr.Attempts) != 1 {
				t.Fatalf("cancelled session moved: %s after %d attempts", tr.Served, len(tr.Attempts))
			}
		})
	}
}

// codexPoolSetup routes local-model to a failover pool of the given Codex
// members, over the two connections of seedTwoConnections.
func codexPoolSetup(members ...string) localSetup {
	l := localSetup{
		Providers: []provider{
			{Name: "codex", Type: "codex", BaseURL: codexBaseURL},
			{Name: "work", Type: "codex", BaseURL: codexBaseURL, AuthID: testAuthB},
		},
		Routes:     map[string]map[string]modelRoute{"local-model": {"default": {Mode: "pool", Pool: "gpt"}}},
		ModelPools: map[string][]poolTarget{"gpt": nil},
	}
	seen := map[string]bool{}
	for _, key := range members {
		if !seen[key] {
			seen[key] = true
			p, m, _ := strings.Cut(key, "/")
			l.Models = append(l.Models, localModel{Provider: p, Model: m})
		}
		l.ModelPools["gpt"] = append(l.ModelPools["gpt"], poolTarget{Model: key})
	}
	return l
}

func codexPoolOf(members ...string) config {
	c := config{local: codexPoolSetup(members...), failover: true, balance: 3, firstByte: 5 * time.Second}
	return c.forModel("local-model", "")
}

func TestCodexSignedOutMemberFailsOver(t *testing.T) {
	served := func(t *testing.T, cfg config, hl *health, session, want string) {
		t.Helper()
		w, tr := runLocalSession(t, cfg, hl, session)
		if w.Code != 200 || tr.Served != want || len(tr.Attempts) != 1 {
			t.Fatalf("%s: %d served %q after %d attempts, want %s in one", session, w.Code, tr.Served, len(tr.Attempts), want)
		}
	}
	t.Run("no credential", func(t *testing.T) {
		useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
		seedConnection(t, provider{Name: "codex", Type: "codex"}, "acct-a")
		codexStatusByAccount(t, map[string]int{})
		cfg, hl := codexPoolOf("work/gpt", "codex/gpt"), newHealth("")
		served(t, cfg, hl, "s1", "codex/gpt")
		served(t, cfg, hl, "s1", "codex/gpt")
		// Signing in brings the connection back for new sessions only.
		seedConnection(t, provider{Name: "work", Type: "codex", AuthID: testAuthB}, "acct-b")
		served(t, cfg, hl, "n1", "work/gpt")
		served(t, cfg, hl, "s1", "codex/gpt")
	})
	t.Run("token rejected", func(t *testing.T) {
		seedTwoConnections(t)
		codexStatusByAccount(t, map[string]int{"acct-b": 401})
		cfg, hl := codexPoolOf("work/gpt", "codex/gpt"), newHealth("")
		w, tr := runLocalSession(t, cfg, hl, "s1")
		if w.Code != 200 || tr.Served != "codex/gpt" || len(tr.Attempts) != 2 {
			t.Fatalf("rejected first member: %d served %q after %d attempts", w.Code, tr.Served, len(tr.Attempts))
		}
		served(t, cfg, hl, "s1", "codex/gpt")
		served(t, cfg, hl, "s2", "codex/gpt")
	})
	t.Run("everyone signed out", func(t *testing.T) {
		useTestCodexHome(t, "http://issuer.invalid", http.DefaultClient)
		codexStatusByAccount(t, map[string]int{})
		cfg, hl := codexPoolOf("work/gpt", "codex/gpt"), newHealth("")
		w, _ := runLocalSession(t, cfg, hl, "s1")
		if w.Code == 200 || !strings.Contains(w.Body.String(), "войдите") {
			t.Fatalf("all signed out: %d %s", w.Code, w.Body.String())
		}
	})
}
