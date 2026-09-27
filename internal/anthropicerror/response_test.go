package anthropicerror

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWriteHTTPOnlyProvenOriginal429GetsRateLimitEnvelope(t *testing.T) {
	for _, tt := range []struct {
		name                          string
		failure                       Failure
		wantType, wantText, wantRetry string
	}{
		{"transient", Failure{429, Transient429, 7 * time.Second, 429}, "rate_limit_error", "The upstream service reported a rate limit.", "7"},
		{"quota", Failure{429, Quota429, 7 * time.Second, 429}, "rate_limit_error", "The upstream service reported a usage or spending limit.", ""},
		{"unknown", Failure{429, Unknown429, 7 * time.Second, 429}, "rate_limit_error", "The upstream service reported a limit.", ""},
		{"mapped 503", Failure{429, Transient429, 7 * time.Second, 503}, "api_error", "The upstream service failed.", ""},
		{"untyped 429", Failure{429, UpstreamFailure, 7 * time.Second, 0}, "api_error", "The upstream service failed.", ""},
		{"invalid duration", Failure{429, Transient429, 25 * time.Hour, 429}, "rate_limit_error", "The upstream service reported a rate limit.", ""},
		{"wrong outward", Failure{502, Transient429, 7 * time.Second, 429}, "api_error", "The upstream service failed.", ""},
		{"untyped status", Failure{0, UpstreamFailure, 0, 0}, "api_error", "The upstream service failed.", ""},
		{"invalid category", Failure{429, Category(255), 0, 429}, "api_error", "The upstream service failed.", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			w.Header()["retry-after"] = []string{"private"}
			w.Header()["Retry-After"] = []string{"other"}
			w.Header().Set("Content-Length", "123456")
			WriteHTTP(w, tt.failure)
			status := tt.failure.Status
			if status < 400 || status > 599 {
				status = http.StatusBadGateway
			}
			if w.Code != status || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Length") != "" || w.Header().Get("Retry-After") != tt.wantRetry || len(w.Header().Values("Retry-After")) > 1 {
				t.Fatalf("status=%d headers=%v", w.Code, w.Header())
			}
			for key := range w.Header() {
				if strings.EqualFold(key, "Retry-After") && key != http.CanonicalHeaderKey(key) {
					t.Fatalf("noncanonical stale retry header survived: %v", w.Header())
				}
			}
			want := `{"type":"error","error":{"type":"` + tt.wantType + `","message":"` + tt.wantText + `"}}` + "\n"
			if w.Body.String() != want {
				t.Fatalf("body = %q; want %q", w.Body.String(), want)
			}
		})
	}
}

func TestWriteSSENeverStartsASecondResponseOrSendsSuccess(t *testing.T) {
	w := httptest.NewRecorder()
	w.WriteHeader(http.StatusOK)
	if err := WriteSSE(w, Failure{429, Transient429, 7 * time.Second, 429}); err != nil {
		t.Fatal(err)
	}
	want := "event: error\ndata: " + `{"type":"error","error":{"type":"rate_limit_error","message":"The upstream service reported a rate limit."}}` + "\n\n"
	if w.Code != http.StatusOK || w.Body.String() != want || !w.Flushed || w.Header().Get("Retry-After") != "" {
		t.Fatalf("committed response = %d, %q, flushed=%v headers=%v", w.Code, w.Body.String(), w.Flushed, w.Header())
	}
	for _, failure := range []Failure{{429, Transient429, 0, 503}, {429, UpstreamFailure, 0, 0}} {
		w := httptest.NewRecorder()
		if err := WriteSSE(w, failure); err != nil || strings.Contains(w.Body.String(), "rate_limit_error") || !strings.Contains(w.Body.String(), `"type":"api_error"`) {
			t.Fatalf("unproven SSE classification: %v %s", err, w.Body.String())
		}
	}
}

type failingSSEWriter struct {
	header  http.Header
	written int
	flushed bool
	err     error
}

func (w *failingSSEWriter) Header() http.Header { return w.header }
func (*failingSSEWriter) WriteHeader(int)       {}
func (w *failingSSEWriter) Flush()              { w.flushed = true }
func (w *failingSSEWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	w.written++
	return len(p) - 1, nil
}

func TestWriteSSEDoesNotFlushIncompleteFrame(t *testing.T) {
	for _, err := range []error{io.ErrClosedPipe, nil} {
		w := &failingSSEWriter{header: make(http.Header), err: err}
		if got := WriteSSE(w, Failure{429, Unknown429, 0, 429}); got == nil || w.flushed {
			t.Fatalf("failed or short write flushed frame: err=%v flushed=%v", got, w.flushed)
		}
		if err != nil && !errors.Is(WriteSSE(w, Failure{429, Unknown429, 0, 429}), io.ErrClosedPipe) {
			t.Fatal("write error lost")
		}
	}
}
