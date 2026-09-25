package main

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFreePortsAreDistinct(t *testing.T) {
	ports, err := freePorts(8)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, port := range ports {
		n, err := strconv.Atoi(port)
		if err != nil || n <= 0 || seen[port] {
			t.Fatalf("bad port %q in %v", port, ports)
		}
		seen[port] = true
	}
	if len(seen) != 8 {
		t.Fatalf("got %v", ports)
	}
}

func TestStreamFlushesEachEventThenSaysDone(t *testing.T) {
	server := httptest.NewServer(streamHandler())
	defer server.Close()
	resp, err := http.Get(server.URL + "/check/stream?events=3&every=300ms")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	start := time.Now()
	first, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || first != "data: 0\n" || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("first event %q after %v: %v", first, time.Since(start), err)
	}
	rest, err := io.ReadAll(resp.Body)
	if err != nil || !strings.Contains(string(rest), "data: 2\n") || !strings.HasSuffix(string(rest), "event: done\ndata: 3\n\n") {
		t.Fatalf("stream %q: %v", rest, err)
	}
}

func TestOtherPathsAreNotFound(t *testing.T) {
	server := httptest.NewServer(streamHandler())
	defer server.Close()
	resp, err := http.Get(server.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
