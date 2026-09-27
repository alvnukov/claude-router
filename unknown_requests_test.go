package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"localrouter/internal/history"
)

func TestUnknownRequestsCapturedWithoutChangingProxy(t *testing.T) {
	requestBody := strings.Repeat("q", debugCaptureLimit+100)
	responseBody := strings.Repeat("r", debugCaptureLimit+200)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != requestBody || r.URL.RawQuery != "token=query-secret" || r.Header.Get("Authorization") != "Bearer header-secret" {
			t.Error("forwarded request changed")
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, responseBody)
	}))
	defer up.Close()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	u, handler := limitsRouterStore(t, up.URL, "", history.New(10, path))
	r := httptest.NewRequest("POST", "/v1/unknown?token=query-secret", strings.NewReader(requestBody))
	r.Header.Set("Authorization", "Bearer header-secret")
	r.Header.Set("Cookie", "cookie-secret")
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot || w.Body.String() != responseBody {
		t.Fatal("response changed")
	}
	reloaded := history.New(10, path).List()
	if len(reloaded) != 1 {
		t.Fatalf("record count: %d", len(reloaded))
	}
	rec := reloaded[0]
	if rec.Method != "POST" || rec.Path != "/v1/unknown" || rec.UnrecognizedReason == "" || !rec.ReqTruncated || !rec.RespTruncated || len(rec.ReqBody) != debugCaptureLimit || len(rec.RespBytes) != debugCaptureLimit {
		t.Fatalf("invalid debug capture: %+v", (uiBackend{u}).requestDTO(rec))
	}
	if len(rec.Headers) != 1 || rec.Headers["Content-Type"] != "text/plain" {
		t.Fatalf("unsafe headers: %v", rec.Headers)
	}
	list := (uiBackend{u}).Requests(url.Values{"unrecognized": {"1"}, "q": {"/v1/unknown"}})
	if list.Total != 1 {
		t.Fatal("unknown path not searchable")
	}
	detail, ok := (uiBackend{u}).Detail(rec.ID)
	if !ok || !detail.CaptureTruncated || len(detail.Response) != debugCaptureLimit || detail.RequestNote == "" {
		t.Fatal("capture details missing")
	}
}

type brokenRequestBody struct{ sent bool }

func (b *brokenRequestBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, errors.New("broken pipe")
	}
	b.sent = true
	return copy(p, `{"model":`), nil
}
func (*brokenRequestBody) Close() error { return nil }

func TestUnrecognizedMessagesAndTokenRequests(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("invalid request reached upstream") }))
	defer up.Close()
	u, h := limitsRouter(t, up.URL, "")
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		for _, body := range []string{`{"broken":`, `{"messages":[]}`, `{"model":"unknown-debug-model","messages":[]}`} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
			if w.Code != http.StatusBadRequest {
				t.Fatal(w.Code)
			}
			rec := u.st.List()[0]
			if rec.UnrecognizedReason == "" || string(rec.ReqBody) != body || rec.Path != path || rec.Status != w.Code {
				t.Fatal("rejected request lost")
			}
		}
		r := httptest.NewRequest("POST", path, nil)
		r.Body = &brokenRequestBody{}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		rec := u.st.List()[0]
		if w.Code != http.StatusBadRequest || !rec.ReqTruncated || string(rec.ReqBody) != `{"model":` || rec.UnrecognizedReason == "" {
			t.Fatal("read failure lost")
		}
	}
	if (uiBackend{u}).Requests(url.Values{"unrecognized": {"1"}, "errors": {"1"}}).Total != 8 {
		t.Fatal("unrecognized/error filters differ")
	}
}

func TestRejectedRequestCaptureIsBounded(t *testing.T) {
	u, h := limitsRouter(t, "http://127.0.0.1:1", "")
	body := strings.Repeat("bad-json", debugCaptureLimit)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	rec := u.st.List()[0]
	if w.Code != http.StatusBadRequest || !rec.ReqTruncated || len(rec.ReqBody) != debugCaptureLimit || rec.RequestNote == "" {
		t.Fatal("rejected capture was not bounded")
	}
}

func TestUnknownRequestBodyStreamsToUpstream(t *testing.T) {
	first := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p [1]byte
		if _, err := io.ReadFull(r.Body, p[:]); err != nil {
			return
		}
		close(first)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()
	u, h := limitsRouter(t, up.URL, "")
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	r := httptest.NewRequest("POST", "/unknown-stream", reader)
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(httptest.NewRecorder(), r) }()
	_, _ = writer.Write([]byte("a"))
	select {
	case <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("router buffered the request before forwarding")
	}
	_, _ = writer.Write([]byte("b"))
	_ = writer.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not finish")
	}
	rec := u.st.List()[0]
	if string(rec.ReqBody) != "ab" || rec.ReqTruncated {
		t.Fatal("stream capture incomplete")
	}
	data, err := json.Marshal(rec)
	if err != nil || !strings.Contains(string(data), "UnrecognizedReason") {
		t.Fatal("debug metadata not serializable")
	}
}
