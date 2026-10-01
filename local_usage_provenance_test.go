package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"localrouter/internal/history"
)

// A missing upstream measurement must survive the synthetic Anthropic usage
// fields in the client response and the history JSON round trip.
func TestGenericUsageProvenanceSurvivesHistoryRestart(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		stream      bool
		known       bool
	}{
		{"blocking missing", "", false, false},
		{"blocking null", `null`, false, false},
		{"blocking empty", `{}`, false, false},
		{"blocking partial", `{"prompt_tokens":7}`, false, false},
		{"blocking explicit zero", `{"prompt_tokens":0,"completion_tokens":0}`, false, true},
		{"streaming missing", "", true, false},
		{"streaming null", `null`, true, false},
		{"streaming empty", `{}`, true, false},
		{"streaming partial", `{"prompt_tokens":7}`, true, false},
		{"streaming explicit zero", `{"prompt_tokens":0,"completion_tokens":0}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/chat/completions" {
					t.Errorf("unexpected upstream path %s", r.URL.Path)
				}
				usageField := ""
				if tc.usage != "" {
					usageField = `,"usage":` + tc.usage
				}
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`+"\n\n")
					fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]`+usageField+"}\n\n")
					fmt.Fprint(w, "data: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]`+usageField+"}")
			}))
			defer upstream.Close()

			path := filepath.Join(t.TempDir(), "history.jsonl")
			store := history.New(10, path)
			body := fmt.Sprintf(`{"model":"local-model","max_tokens":10,"stream":%t,"messages":[{"role":"user","content":"hi"}]}`, tc.stream)
			record := &history.Record{Path: "/v1/messages", Start: time.Now(), ReqBody: []byte(body)}
			store.Add(record)
			writer := history.NewRecorder(httptest.NewRecorder(), 1<<20)
			trace := &history.Trace{}
			handleLocal(writer, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)), config{Local: oneProvider(upstream.URL, "model"), FirstByte: time.Second}, []byte(body), trace, newHealth(""))
			store.Finish(record.ID, writer, trace)
			if got := store.Get(record.ID); got.Status != http.StatusOK || got.Failed() {
				t.Fatalf("router response status=%d failed=%v", got.Status, got.Failed())
			}
			loaded := history.New(10, path).List()
			if len(loaded) != 1 {
				t.Fatalf("history rows after restart = %d", len(loaded))
			}
			encoded, err := json.Marshal(loaded[0])
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			want := "false"
			if tc.known {
				want = "true"
			}
			if got := string(fields["UsageKnown"]); got != want {
				t.Fatalf("persisted upstream usage provenance = %q, want %s; response=%s", got, want, writer.Header().Get("Content-Type"))
			}
		})
	}
}
