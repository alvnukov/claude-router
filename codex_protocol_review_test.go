package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"localrouter/internal/history"
)

func TestCodexReviewDoesNotFailoverAfterGenerationHasStarted(t *testing.T) {
	for _, tt := range []struct {
		name, started string
		stream        bool
	}{
		{"buffered text", `data: {"type":"response.output_text.delta","delta":"billable partial answer"}` + "\n\n", false},
		{"streaming opaque reasoning", `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs","summary":[],"encrypted_content":"already-generated"}}` + "\n\n", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seedTwoConnections(t)
			old := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = old })
			calls := 0
			http.DefaultTransport = usageTransport(func(*http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					failure := `data: {"type":"response.failed","response":{"id":"r1","error":{"code":"rate_limit_exceeded"}}}` + "\n\n"
					return usageResponse(200, tt.started+failure), nil
				}
				return usageResponse(200, codexStreamOK), nil
			})
			response, trace := runLocalRequest(t, context.Background(), twoCodexPool(), newHealth(""), "review-session", tt.stream)
			if calls != 1 || len(trace.Attempts) != 1 {
				t.Fatalf("already-started generation was billed again via failover: calls=%d attempts=%d status=%d", calls, len(trace.Attempts), response.Code)
			}
			if response.Code == http.StatusOK {
				t.Fatalf("failed generation reported success: %s", response.Body.String())
			}
		})
	}
}

func TestCodexReviewDoesNotRestartCompletedContinuationOnNextCallRateLimit(t *testing.T) {
	seedTwoConnections(t)
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	calls := 0
	http.DefaultTransport = usageTransport(func(*http.Request) (*http.Response, error) {
		calls++
		switch calls {
		case 1:
			return usageResponse(200, `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs","summary":[],"encrypted_content":"paid-state"}}`+"\n\n"+
				`data: {"type":"response.completed","response":{"id":"r1","end_turn":false,"usage":{"input_tokens":100,"output_tokens":30}}}`+"\n\n"), nil
		case 2:
			return usageResponse(http.StatusTooManyRequests, `{"error":{"code":"rate_limit_exceeded"}}`), nil
		default:
			return usageResponse(200, codexStreamOK), nil
		}
	})
	response, trace := runLocalRequest(t, context.Background(), twoCodexPool(), newHealth(""), "review-continuation", false)
	if calls != 2 || len(trace.Attempts) != 1 || response.Code == http.StatusOK {
		t.Fatalf("completed sampling restarted on another account: calls=%d attempts=%d status=%d", calls, len(trace.Attempts), response.Code)
	}
}

type codexReviewDisconnectedWriter struct{ header http.Header }

func (w *codexReviewDisconnectedWriter) Header() http.Header { return w.header }
func (*codexReviewDisconnectedWriter) WriteHeader(int)       {}
func (*codexReviewDisconnectedWriter) Flush()                {}
func (*codexReviewDisconnectedWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func TestCodexReviewDoesNotCaptureUndeliveredStream(t *testing.T) {
	seedTwoConnections(t)
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = usageTransport(func(*http.Request) (*http.Response, error) {
		return usageResponse(200, codexStreamOK), nil
	})
	body := []byte(`{"model":"local-model","stream":true,"metadata":{"user_id":"{\"session_id\":\"review\"}"},"messages":[{"role":"user","content":"hi"}]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
	trace := &history.Trace{}
	writer := &codexReviewDisconnectedWriter{header: make(http.Header)}
	handleLocal(writer, request, twoCodexPool(), body, trace, newHealth(""))
	if len(trace.ProviderState) != 0 {
		t.Fatalf("undelivered output became trusted replay state (%d bytes)", len(trace.ProviderState))
	}
	if len(trace.Attempts) != 1 || trace.Attempts[0].Err == "" {
		t.Fatalf("client write failure was not propagated: %+v", trace.Attempts)
	}
}
