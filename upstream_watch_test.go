package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
