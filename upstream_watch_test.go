package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedCodex answers each Codex request with headers at once and a body
// the connection's script writes. A script that returns without closing the
// body leaves the upstream silent; the body closes when the router cancels
// the request, as a real transport's does.
func scriptedCodex(t *testing.T, scripts map[string]func(w *io.PipeWriter)) *sync.Map {
	t.Helper()
	calls := &sync.Map{}
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "chatgpt.com" {
			return usageResponse(400, `{"error":"invalid_grant"}`), nil
		}
		acct := r.Header.Get("ChatGPT-Account-Id")
		n, _ := calls.LoadOrStore(acct, new(atomic.Int32))
		n.(*atomic.Int32).Add(1)
		script := scripts[acct]
		if script == nil {
			t.Errorf("unexpected request on %s", acct)
			return usageResponse(500, `{"error":{"message":"unexpected"}}`), nil
		}
		pr, pw := io.Pipe()
		ctx := r.Context()
		context.AfterFunc(ctx, func() { pw.CloseWithError(context.Cause(ctx)) })
		go script(pw)
		resp := usageResponse(200, "")
		resp.Body = pr
		return resp, nil
	})
	return calls
}

func sse(w io.Writer, data string) {
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func answerOK(w *io.PipeWriter) {
	_, _ = io.WriteString(w, codexStreamOK)
	w.Close()
}

func called(calls *sync.Map, acct string) int32 {
	n, ok := calls.Load(acct)
	if !ok {
		return 0
	}
	return n.(*atomic.Int32).Load()
}

// lastEvent returns the name and data of the last SSE event in body.
func lastEvent(t *testing.T, body string) (string, string) {
	t.Helper()
	chunks := strings.Split(strings.TrimSpace(body), "\n\n")
	name, data, _ := strings.Cut(chunks[len(chunks)-1], "\n")
	return strings.TrimPrefix(name, "event: "), strings.TrimPrefix(data, "data: ")
}

// The incident: the model sends a delta, then the upstream goes silent with
// the connection open. The client must get an error event it retries, not a
// stream that hangs until someone presses Esc.
func TestCodexIdleAfterCommitEndsWithError(t *testing.T) {
	seedTwoConnections(t)
	cfg, hl := twoCodexPool(), newHealth("")
	cfg.startTimeout, cfg.idleTimeout = 0, 50*time.Millisecond
	calls := scriptedCodex(t, map[string]func(*io.PipeWriter){
		"acct-a": func(w *io.PipeWriter) {
			sse(w, `{"type":"response.created"}`)
			sse(w, `{"type":"response.output_text.delta","delta":"half"}`)
		},
	})
	w, tr := runLocalRequest(t, context.Background(), cfg, hl, "s1", true)

	body := w.Body.String()
	if !strings.HasPrefix(body, "event: message_start") || !strings.Contains(body, `"text":"half"`) {
		t.Fatalf("client did not get the start and the delta:\n%s", body)
	}
	name, data := lastEvent(t, body)
	var e struct {
		Type  string
		Error struct{ Type, Message string }
	}
	if err := json.Unmarshal([]byte(data), &e); err != nil || name != "error" || e.Type != "error" {
		t.Fatalf("stream does not end with an error event: %q %q", name, data)
	}
	if e.Error.Type != "overloaded_error" || e.Error.Message != "codex/gpt: upstream idle 50ms" {
		t.Fatalf("error event: %+v", e.Error)
	}
	if n := called(calls, "acct-b"); n != 0 {
		t.Fatalf("answer already started, yet retried on the other member %d times", n)
	}
	if len(tr.Attempts) != 1 {
		t.Fatalf("attempts: %+v", tr.Attempts)
	}
	if a := tr.Attempts[0]; a.Outcome != "upstream_idle" || a.MaxGap < 50*time.Millisecond {
		t.Fatalf("attempt: %+v", a)
	}
	if st := hl.snapshot("codex/gpt"); st.Fail != 1 {
		t.Fatalf("idle member not charged with a failure: %+v", st)
	}
}

// Reasoning summaries carry no text for the client, but they are bytes from a
// live model: a long think that sends them is not idle.
func TestCodexSummariesKeepStreamAlive(t *testing.T) {
	seedTwoConnections(t)
	cfg, hl := twoCodexPool(), newHealth("")
	const idle = 250 * time.Millisecond
	cfg.startTimeout, cfg.idleTimeout = idle, idle
	scriptedCodex(t, map[string]func(*io.PipeWriter){
		"acct-a": func(w *io.PipeWriter) {
			tick := time.NewTicker(20 * time.Millisecond)
			defer tick.Stop()
			end := time.Now().Add(3 * idle)
			for now := range tick.C {
				if now.After(end) {
					break
				}
				sse(w, `{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`)
			}
			sse(w, `{"type":"response.output_text.delta","delta":"done"}`)
			sse(w, `{"type":"response.completed","response":{"status":"completed"}}`)
			w.Close()
		},
	})
	w, tr := runLocalRequest(t, context.Background(), cfg, hl, "s1", true)

	body := w.Body.String()
	if name, _ := lastEvent(t, body); name != "message_stop" || strings.Contains(body, "thinking") {
		t.Fatalf("long think did not end cleanly, or the summary reached the client:\n%s", body)
	}
	if a := tr.Attempts[0]; len(tr.Attempts) != 1 || a.Outcome != "ok" || a.MaxGap <= 0 || a.MaxGap >= idle {
		t.Fatalf("attempts: %+v", tr.Attempts)
	}
}

// Headers, then nothing: the member never started, so the next one may try.
func TestCodexStartTimeoutFailsOver(t *testing.T) {
	seedTwoConnections(t)
	cfg, hl := twoCodexPool(), newHealth("")
	cfg.startTimeout, cfg.idleTimeout = 50*time.Millisecond, time.Minute
	calls := scriptedCodex(t, map[string]func(*io.PipeWriter){
		"acct-a": func(*io.PipeWriter) {},
		"acct-b": answerOK,
	})
	w, tr := runLocalRequest(t, context.Background(), cfg, hl, "s1", true)

	if called(calls, "acct-b") != 1 || tr.Served != "work/gpt" || w.Code != 200 {
		t.Fatalf("no failover after a silent start: %d served %q", w.Code, tr.Served)
	}
	if len(tr.Attempts) != 2 {
		t.Fatalf("attempts: %+v", tr.Attempts)
	}
	first := tr.Attempts[0]
	if first.Outcome != "upstream_idle" || first.MaxGap != 50*time.Millisecond || !strings.Contains(first.Err, "upstream start 50ms") {
		t.Fatalf("first attempt: %+v", first)
	}
	if tr.Attempts[1].Outcome != "ok" {
		t.Fatalf("second attempt: %+v", tr.Attempts[1])
	}
}

// Summaries, then silence before any text: the member went quiet mid-think.
// Another full idle wait on the next member would double the client's wait,
// so the client gets the error now and the session moves for its retry.
func TestCodexIdleBeforeFirstEventDoesNotRetry(t *testing.T) {
	seedTwoConnections(t)
	cfg, hl := twoCodexPool(), newHealth("")
	cfg.startTimeout, cfg.idleTimeout = time.Minute, 100*time.Millisecond
	calls := scriptedCodex(t, map[string]func(*io.PipeWriter){
		"acct-a": func(w *io.PipeWriter) {
			for range 3 {
				sse(w, `{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`)
			}
		},
		"acct-b": answerOK,
	})
	w, tr := runLocalRequest(t, context.Background(), cfg, hl, "s1", true)

	if n := called(calls, "acct-b"); n != 0 {
		t.Fatalf("idle member retried on the other one %d times", n)
	}
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), `"type":"api_error"`) || !strings.Contains(w.Body.String(), "upstream idle 100ms") {
		t.Fatalf("client got %d %s", w.Code, w.Body.String())
	}
	if len(tr.Attempts) != 1 || tr.Attempts[0].Outcome != "upstream_idle" {
		t.Fatalf("attempts: %+v", tr.Attempts)
	}
	if st := hl.snapshot("codex/gpt"); st.Fail != 1 {
		t.Fatalf("idle member not charged with a failure: %+v", st)
	}
	if _, tr := runLocalRequest(t, context.Background(), cfg, hl, "s1", true); tr.Served != "work/gpt" || len(tr.Attempts) != 1 {
		t.Fatalf("the retry of the session went to %q after %d attempts", tr.Served, len(tr.Attempts))
	}
}

// The upstream hangs up after a delta without response.completed.
func TestCodexClosedAfterCommitEndsWithError(t *testing.T) {
	seedTwoConnections(t)
	cfg, hl := twoCodexPool(), newHealth("")
	cfg.startTimeout, cfg.idleTimeout = time.Minute, time.Minute
	scriptedCodex(t, map[string]func(*io.PipeWriter){
		"acct-a": func(w *io.PipeWriter) {
			sse(w, `{"type":"response.output_text.delta","delta":"half"}`)
			w.Close()
		},
	})
	w, tr := runLocalRequest(t, context.Background(), cfg, hl, "s1", true)

	name, data := lastEvent(t, w.Body.String())
	if name != "error" || !strings.Contains(data, `"type":"overloaded_error"`) ||
		!strings.Contains(data, `"message":"codex/gpt: Codex stream ended before response.completed"`) {
		t.Fatalf("last event %q %s", name, data)
	}
	if len(tr.Attempts) != 1 || tr.Attempts[0].Outcome != "upstream_closed" {
		t.Fatalf("attempts: %+v", tr.Attempts)
	}
}

// Without summaries a long think sends no bytes and looks idle.
func TestCodexRequestAsksForSummaries(t *testing.T) {
	for _, effort := range []string{"", "high"} {
		creq, err := toCodex(openaiRequest{Model: "gpt", ReasoningEffort: effort})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(creq)
		var got struct {
			Reasoning map[string]string `json:"reasoning"`
		}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"summary": "auto"}
		if effort != "" {
			want["effort"] = effort
		}
		if fmt.Sprint(got.Reasoning) != fmt.Sprint(want) {
			t.Fatalf("effort %q: reasoning %s", effort, b)
		}
	}
}

func TestWatchBodyFiresOnce(t *testing.T) {
	pr, pw := io.Pipe()
	var aborts atomic.Int32
	b := watchBody(pr, 0, 20*time.Millisecond, func(err error) { aborts.Add(1); pr.CloseWithError(err) })
	go func() { _, _ = pw.Write([]byte("x")) }()
	p := make([]byte, 8)
	if n, err := b.Read(p); n != 1 || err != nil {
		t.Fatalf("first read: %d %v", n, err)
	}
	_, err := b.Read(p)
	if !errors.Is(err, errUpstreamIdle) || err.Error() != "upstream idle 20ms" {
		t.Fatalf("read after the pause: %v", err)
	}
	if _, again := b.Read(p); again != err {
		t.Fatalf("later read: %v", again)
	}
	b.Close()
	if n := aborts.Load(); n != 1 {
		t.Fatalf("abort called %d times", n)
	}
	if b.MaxGap() != 20*time.Millisecond {
		t.Fatalf("max gap %s", b.MaxGap())
	}
}

// Run with -race: Close, Read and the timer share the watch's state.
func TestWatchBodyCloseRacesTimer(t *testing.T) {
	for range 50 {
		pr, pw := io.Pipe()
		b := watchBody(pr, time.Microsecond, time.Microsecond, func(error) { pr.Close() })
		done := make(chan struct{})
		go func() {
			defer close(done)
			p := make([]byte, 1)
			for {
				if _, err := b.Read(p); err != nil {
					return
				}
			}
		}()
		go func() { _, _ = pw.Write([]byte("xy")) }()
		b.Close()
		<-done
		_ = b.MaxGap()
	}
}

// On the Anthropic path the router adds no event of its own: an upstream
// that goes silent after message_start is cut off, and the client's
// connection closes right after the last byte it got.
func TestAnthropicIdleClosesClientConnection(t *testing.T) {
	captureLog(t)
	upstreamGone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamGone)
	}))
	defer upstream.Close()
	_, handler := limitsRouterStore(t, upstream.URL, "", newStore(10, ""), func(c *config) {
		c.startTimeout, c.idleTimeout = 0, 50*time.Millisecond
	})
	router := httptest.NewServer(handler)
	defer router.Close()

	body := `{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := (&http.Client{Transport: &http.Transport{}}).Post(router.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	type read struct {
		body []byte
		err  error
	}
	done := make(chan read, 1)
	go func() {
		b, err := io.ReadAll(resp.Body)
		done <- read{b, err}
	}()
	var got read
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("client stream still open 5s after the upstream went silent")
	}
	if resp.StatusCode != http.StatusOK || string(got.body) != "event: message_start\ndata: {}\n\n" {
		t.Fatalf("status %d, client got %q; want message_start and nothing after", resp.StatusCode, got.body)
	}
	if got.err == nil {
		t.Fatal("stream ended cleanly; want the connection cut")
	}
	select {
	case <-upstreamGone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request still open")
	}
}

// A protocol upgrade goes through unwatched: the proxy needs the upstream's
// own read-write body, and a quiet upgraded connection is not a silent answer.
func TestProxyUpgradeIsNotWatched(t *testing.T) {
	captureLog(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "echo" {
			http.Error(w, "want an upgrade", http.StatusBadRequest)
			return
		}
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		line, _ := buf.ReadString('\n')
		_, _ = io.WriteString(conn, line)
	}))
	defer upstream.Close()
	_, handler := limitsRouterStore(t, upstream.URL, "", newStore(10, ""), func(c *config) {
		c.startTimeout, c.idleTimeout = 0, 50*time.Millisecond
	})
	router := httptest.NewServer(handler)
	defer router.Close()

	conn, err := net.Dial("tcp", router.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(conn, "GET /echo HTTP/1.1\r\nHost: router\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d %q; want 101", resp.StatusCode, b)
	}
	_, _ = io.WriteString(conn, "ping\n")
	if line, err := br.ReadString('\n'); line != "ping\n" {
		t.Fatalf("echo %q, %v; want ping", line, err)
	}
}

// A client that did not ask for a stream waits for the whole answer, and its
// headers or any byte may come only when the answer is done. None of the
// bounds on a silent upstream may cut it: before them it worked however long
// it took. The whole answer here keeps silent past the header bound, and past
// the start and then the idle bound; each time a streamed request, cut at the
// same bound, is the clock that shows the silence outlasted it. Codex is the
// exception: the router reads it as a stream whatever the client asked for.
func TestNonStreamNotBoundedByStart(t *testing.T) {
	t.Run("headers", func(t *testing.T) {
		arrived, release := make(chan struct{}, 1), make(chan struct{})
		free := sync.OnceFunc(func() { close(release) })
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body) // net/http notices a gone client only after the body
			if r.URL.Path == "/stream" {
				<-r.Context().Done()
				return
			}
			arrived <- struct{}{}
			select {
			case <-release:
				_, _ = io.WriteString(w, `{}`)
			case <-r.Context().Done():
			}
		}))
		defer upstream.Close()
		defer free()
		tr := upstreamTransport()
		tr.ResponseHeaderTimeout = 50 * time.Millisecond
		rt := byStream(tr)

		type result struct {
			resp *http.Response
			err  error
		}
		whole := make(chan result, 1)
		go func() {
			req, _ := http.NewRequest("POST", upstream.URL+"/whole", strings.NewReader(`{}`))
			resp, err := rt.RoundTrip(req)
			whole <- result{resp, err}
		}()
		<-arrived
		req, _ := http.NewRequestWithContext(markStream(context.Background(), true), "POST", upstream.URL+"/stream", strings.NewReader(`{}`))
		resp, err := rt.RoundTrip(req)
		if err == nil {
			resp.Body.Close()
			t.Fatal("streamed request got headers that were never sent")
		}
		if !strings.Contains(err.Error(), "timeout awaiting response headers") {
			t.Fatalf("streamed request failed, but not at the header bound: %v", err)
		}
		free()
		got := <-whole
		if got.err != nil {
			t.Fatalf("whole answer slower than the header bound: %v", got.err)
		}
		body, err := readAllWithin(t, got.resp.Body)
		got.resp.Body.Close()
		if got.resp.StatusCode != http.StatusOK || err != nil || string(body) != `{}` {
			t.Fatalf("whole answer: status %d, %q, %v", got.resp.StatusCode, body, err)
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		captureLog(t)
		const head, tail = `{"type":`, `"message"}`
		first, rest := make(chan struct{}), make(chan struct{})
		freeFirst, freeRest := sync.OnceFunc(func() { close(first) }), sync.OnceFunc(func() { close(rest) })
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			stream := strings.Contains(string(body), `"stream":true`)
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
			} else {
				w.Header().Set("Content-Type", "application/json")
			}
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			if stream {
				<-r.Context().Done()
				return
			}
			select {
			case <-first:
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, head)
			w.(http.Flusher).Flush()
			select {
			case <-rest:
				_, _ = io.WriteString(w, tail)
			case <-r.Context().Done():
			}
		}))
		defer upstream.Close()
		defer freeRest()
		defer freeFirst()
		marks := recordStreamMarks(t)
		const bound = 50 * time.Millisecond
		_, handler := limitsRouterStore(t, upstream.URL, "", newStore(10, ""), func(c *config) {
			c.startTimeout, c.idleTimeout = bound, bound
		})
		router := httptest.NewServer(handler)
		defer router.Close()
		client := &http.Client{Transport: &http.Transport{}}
		post := func(stream bool) *http.Response {
			t.Helper()
			body := fmt.Sprintf(`{"model":"claude-sonnet-5","stream":%t,"messages":[{"role":"user","content":"hi"}]}`, stream)
			resp, err := client.Post(router.URL+"/v1/messages", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			return resp
		}

		clock := func() {
			t.Helper()
			began := time.Now()
			resp := post(true)
			defer resp.Body.Close()
			if _, err := readAllWithin(t, resp.Body); err == nil || time.Since(began) < bound {
				t.Fatalf("streamed answer not cut at the bound: %v after %s", err, time.Since(began))
			}
		}

		whole := post(false)
		defer whole.Body.Close()
		clock()
		freeFirst()
		got, err := readAllWithin(t, io.LimitReader(whole.Body, int64(len(head))))
		if string(got) != head || err != nil {
			t.Fatalf("whole answer cut before its first byte: %q, %v", got, err)
		}
		clock()
		freeRest()
		body, err := readAllWithin(t, whole.Body)
		if whole.StatusCode != http.StatusOK || err != nil || string(body) != tail {
			t.Fatalf("whole answer cut after its first byte: status %d, %q, %v", whole.StatusCode, body, err)
		}
		if got := marks(); !slices.Equal(got, []bool{false, true, true}) {
			t.Fatalf("requests through upstreamHTTP marked as streamed: %v, want [false true true]", got)
		}
	})

	// Codex streams to the router even for a whole answer, and a live model
	// keeps sending events and summaries, so its silence is a stall as on a
	// stream: silent from the start it moves to the next member, silent after
	// it began it ends in a 502 with no retry.
	t.Run("codex", func(t *testing.T) {
		seedTwoConnections(t)
		cfg, hl := twoCodexPool(), newHealth("")
		cfg.startTimeout, cfg.idleTimeout = 50*time.Millisecond, 50*time.Millisecond
		head := codexStreamOK[:strings.Index(codexStreamOK, "\n\n")+2]
		calls := scriptedCodex(t, map[string]func(*io.PipeWriter){
			"acct-a": func(*io.PipeWriter) {},
			"acct-b": func(w *io.PipeWriter) { _, _ = io.WriteString(w, head) },
		})
		marks := recordStreamMarks(t)

		type run struct {
			w  *httptest.ResponseRecorder
			tr *localTrace
		}
		whole := make(chan run, 1)
		go func() {
			w, tr := runLocalRequest(t, context.Background(), cfg, hl, "s1", false)
			whole <- run{w, tr}
		}()
		var got run
		select {
		case got = <-whole:
		case <-time.After(5 * time.Second):
			t.Fatal("whole Codex answer still waits on a silent upstream after 5s")
		}
		if got.w.Code != http.StatusBadGateway || !strings.Contains(got.w.Body.String(), "upstream idle 50ms") {
			t.Fatalf("whole answer: status %d\n%s", got.w.Code, got.w.Body.String())
		}
		if called(calls, "acct-a") != 1 || called(calls, "acct-b") != 1 || len(got.tr.Attempts) != 2 {
			t.Fatalf("calls a %d b %d, attempts %+v", called(calls, "acct-a"), called(calls, "acct-b"), got.tr.Attempts)
		}
		for i, bound := range []string{"upstream start 50ms", "upstream idle 50ms"} {
			if a := got.tr.Attempts[i]; a.Outcome != "upstream_idle" || !strings.Contains(a.Err, bound) {
				t.Fatalf("attempt %d: %+v; want upstream_idle at %q", i, a, bound)
			}
		}
		if got := marks(); !slices.Equal(got, []bool{true, true}) {
			t.Fatalf("requests through upstreamHTTP marked as streamed: %v, want [true true]", got)
		}
	})
}

// readAllWithin reads r to its end, failing the test if that takes 5s.
func readAllWithin(t *testing.T, r io.Reader) ([]byte, error) {
	t.Helper()
	type read struct {
		b   []byte
		err error
	}
	done := make(chan read, 1)
	go func() {
		b, err := io.ReadAll(r)
		done <- read{b, err}
	}()
	select {
	case got := <-done:
		return got.b, got.err
	case <-time.After(5 * time.Second):
		t.Fatal("body still open after 5s")
		return nil, nil
	}
}
