package main

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// passLines is the catch-all's lines in the captured log.
func passLines(log string) []string {
	var lines []string
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "pass ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// Whatever the catch-all forwards gets one log line: method, path without the
// query, status and duration. Headers, query and bodies stay out of it, and
// the request and response pass through unchanged.
func TestCatchAllLogsOneLine(t *testing.T) {
	buf := captureLog(t)
	seen := make(chan string, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen <- r.Method + " " + r.URL.RequestURI() + " " + r.Header.Get("Authorization") + " " + string(b)
		w.Header().Set("X-Upstream", "resp-header-secret")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "resp-body-secret")
	}))
	defer upstream.Close()
	_, handler := limitsRouter(t, upstream.URL, "")

	req := httptest.NewRequest("POST", "/api/oauth/profile?token=query-secret", strings.NewReader("req-body-secret"))
	req.Header.Set("Authorization", "Bearer header-secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusTeapot || w.Body.String() != "resp-body-secret" || w.Header().Get("X-Upstream") != "resp-header-secret" {
		t.Fatalf("client got %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	if got := <-seen; got != "POST /api/oauth/profile?token=query-secret Bearer header-secret req-body-secret" {
		t.Fatalf("upstream got a different request: %q", got)
	}
	lines := passLines(buf.String())
	if len(lines) != 1 || !regexp.MustCompile(`^pass POST /api/oauth/profile -> 418 in \S+$`).MatchString(lines[0]) {
		t.Fatalf("log lines %q", lines)
	}
	if strings.Contains(buf.String(), "secret") {
		t.Fatalf("log carries request or response data:\n%s", buf.String())
	}

	// A path cannot break the line or grow it without bound.
	for _, c := range []struct{ path, want string }{
		{"/v1/x%0Aforged%20line", `^pass GET /v1/x%0Aforged%20line -> 418 in \S+$`},
		{"/" + strings.Repeat("a", 2000), `^pass GET /a+… -> 418 in \S+$`},
	} {
		buf.Reset()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", c.path, nil))
		lines := passLines(buf.String())
		if len(lines) != 1 || len(lines[0]) > 320 || !regexp.MustCompile(c.want).MatchString(lines[0]) {
			t.Fatalf("%.40s: log %q", c.path, buf.String())
		}
	}

	// Messages keep their own log line; the catch-all line is not added.
	buf.Reset()
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`)))
	if lines := passLines(buf.String()); len(lines) != 0 {
		t.Fatalf("messages got a catch-all line: %q", lines)
	}

	upstream.Close()
	buf.Reset()
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/hello", nil))
	if lines := passLines(buf.String()); w.Code != http.StatusBadGateway || len(lines) != 1 || !regexp.MustCompile(`^pass GET /api/hello -> 502 in \S+$`).MatchString(lines[0]) {
		t.Fatalf("unreachable upstream: %d %q", w.Code, buf.String())
	}
}

// Logging the status must not buffer a streamed catch-all response.
func TestCatchAllStreams(t *testing.T) {
	captureLog(t)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		_, _ = io.WriteString(w, "data: two\n\n")
	}))
	defer upstream.Close()
	_, handler := limitsRouter(t, upstream.URL, "")
	router := httptest.NewServer(handler)
	defer router.Close()

	resp, err := (&http.Client{Transport: &http.Transport{}}).Get(router.URL + "/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	if first, err := rd.ReadString('\n'); err != nil || first != "data: one\n" {
		t.Fatalf("first event before upstream finished: %q %v", first, err)
	}
	close(release)
	if rest, _ := io.ReadAll(rd); string(rest) != "\ndata: two\n\n" {
		t.Fatalf("rest %q", rest)
	}
}
