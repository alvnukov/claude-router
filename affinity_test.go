package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"localrouter/internal/history"
)

func TestSessionAffinityTimeoutAndEffortPools(t *testing.T) {
	var stallA atomic.Bool
	var outside atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openaiRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if req.Model == "outside" {
			outside.Add(1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if req.Model == "a" && stallA.Load() {
			// A header, ping and role announcement must not end the start timeout.
			fmt.Fprint(w, "event: ping\ndata: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", req.Model+":"+req.ReasoningEffort)
	}))
	defer server.Close()
	l := oneProvider(server.URL, "a", "b", "outside")
	l.ModelPools = map[string][]poolTarget{"deep": {{Model: "p/a", Effort: "xhigh"}, {Model: "p/b", Effort: "medium"}}, "fast": {{Model: "p/b", Effort: "low"}}}
	l.Routes = map[string]map[string]modelRoute{
		"opus":   {"high": {Mode: "pool", Pool: "deep"}, "low": {Mode: "pool", Pool: "fast"}},
		"sonnet": {"high": {Mode: "pool", Pool: "deep"}},
	}
	cfg := config{local: l, failover: true, balance: 2, firstByte: 80 * time.Millisecond}
	hl := newHealth("")
	run := func(session, model, effort, want string, attempts int) {
		t.Helper()
		uid, _ := json.Marshal(map[string]string{"session_id": session})
		body := []byte(fmt.Sprintf(`{"model":%q,"output_config":{"effort":%q},"stream":true,"metadata":{"user_id":%q},"messages":[{"role":"user","content":"hello"}]}`, model, effort, string(uid)))
		w := httptest.NewRecorder()
		tr := &history.Trace{}
		handleLocal(w, httptest.NewRequest("POST", "/v1/messages", nil), cfg.forModel(model, effort), body, tr, hl)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"text":"`+want+`"`) || len(tr.Attempts) != attempts {
			t.Fatalf("%s/%s/%s: %d attempts=%+v body=%s", session, model, effort, w.Code, tr.Attempts, w.Body.String())
		}
	}
	run("one", "opus", "high", "a:xhigh", 1)
	hl.acquire("p/a")
	run("one", "opus", "high", "a:xhigh", 1) // pinned under load
	run("two", "opus", "high", "a:xhigh", 1) // a failover pool keeps pool order under load
	run("one", "opus", "low", "b:low", 1)    // independent effort and target effort
	hl.release("p/a")
	stallA.Store(true)
	run("one", "opus", "high", "b:medium", 2) // timeout after headers -> next model
	stallA.Store(false)
	for i := 0; i < 5; i++ {
		hl.record("p/a", true, time.Millisecond, "")
	}
	run("one", "opus", "high", "b:medium", 1)  // recovery must not steal the session back
	run("one", "sonnet", "high", "a:xhigh", 1) // independent incoming model binds on its own
	cfg.local.ModelPools["deep"] = append(cfg.local.ModelPools["deep"], poolTarget{Model: "p/outside", Effort: "low"})
	run("one", "opus", "high", "b:medium", 1) // catalog growth must not reset affinity

	if outside.Load() != 0 {
		t.Fatal("request escaped selected pool")
	}
	// Changing the pool invalidates the binding; removed models cannot be used.
	cfg.local.ModelPools["deep"] = []poolTarget{{Model: "p/a", Effort: "high"}}
	run("one", "opus", "high", "a:high", 1)
}

func TestConcurrentSessionFirstChoice(t *testing.T) {
	hl := newHealth("")
	var wg sync.WaitGroup
	results := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := []candidate{{Key: "a"}, {Key: "b"}}
			if i%2 == 0 {
				c[0], c[1] = c[1], c[0]
			}
			results <- hl.bindCandidates("session", poolRoute{}, c)[0].Key
		}(i)
	}
	wg.Wait()
	close(results)
	first := ""
	for key := range results {
		if first == "" {
			first = key
		}
		if key != first {
			t.Fatal("concurrent session split across models")
		}
	}
}

func TestRouterStreamsBeforeUpstreamCompletes(t *testing.T) {
	for _, field := range []string{"content", "reasoning_content"} {
		t.Run(field, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{%q:\"first\"}}]}\n\n", field)
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"last\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer upstream.Close()
			cfg := config{local: oneProvider(upstream.URL, "a"), failover: true, firstByte: time.Second}
			router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				handleLocal(w, r, cfg, b, nil, newHealth(""))
			}))
			defer router.Close()
			defer unblock()
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Post(router.URL, "application/json", strings.NewReader(`{"model":"opus","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			scanner := bufio.NewScanner(resp.Body)
			found := false
			for scanner.Scan() {
				if strings.Contains(scanner.Text(), `"text":"first"`) || strings.Contains(scanner.Text(), `"thinking":"first"`) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("first delta buffered: %v", scanner.Err())
			}
			unblock()
			tail, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(tail), "last") || !strings.Contains(string(tail), "message_stop") {
				t.Fatalf("missing stream tail: %s", tail)
			}
		})
	}
}

func TestCandidateSelectionStableForLegacy(t *testing.T) {
	data, _ := json.Marshal(provider{Name: "codex", Type: "codex", BaseURL: codexBaseURL})
	if strings.Contains(string(data), "auth_id") {
		t.Fatal("empty auth_id serialized; legacy bindings would change")
	}
}
