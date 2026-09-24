package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTwoStoresDoNotLoseHistoryDuringCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	old, next := newStore(3, path), newStore(3, path)
	life := newLifecycle(false)
	if err := life.quiesce(); err != nil {
		t.Fatal(err)
	}
	old.life, next.life = life, newLifecycle(false)
	var wg sync.WaitGroup
	for _, s := range []*store{old, next} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 30 {
				r := &record{ID: fmt.Sprintf("%p-%d", s, i), End: time.Now()}
				s.mu.Lock()
				s.recs = append(s.recs, r)
				s.mu.Unlock()
				s.persist(r)
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("corrupt JSONL record: %v", err)
		}
		seen[r.ID] = true
	}
	if len(seen) != 60 {
		t.Fatalf("history lost overlap records: got %d, want 60", len(seen))
	}
}

func TestTwoAuthStoresRefreshRotatingTokenOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex-auth.json")
	var c codexCredential
	c.AuthMode = "chatgpt"
	c.Tokens.AccountID = "account"
	c.Tokens.AccessToken = testJWT(time.Now().Add(-time.Minute))
	c.Tokens.RefreshToken = "original"
	data, _ := json.Marshal(c)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	newToken := testJWT(time.Now().Add(time.Hour))
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.FormValue("refresh_token") != "original" {
			http.Error(w, "rotated token used twice", http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"rotated"}`, newToken)
	}))
	defer issuer.Close()
	a, b := newCodexAuthStore(), newCodexAuthStore()
	for _, s := range []*codexAuthStore{a, b} {
		s.path, s.issuer, s.client = path, issuer.URL, issuer.Client()
	}
	var wg sync.WaitGroup
	for _, s := range []*codexAuthStore{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.credentialFor(context.Background())
			if err != nil || got.Tokens.AccessToken != newToken {
				t.Errorf("rotated credential: %v %+v", err, got)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh requests = %d, want 1", calls.Load())
	}
	saved, err := readCodexCredential(path)
	if err != nil || saved.Tokens.RefreshToken != "rotated" {
		t.Fatalf("invalid saved token: %v %+v", err, saved)
	}
}

func TestQuiescedAuthReadsDiskButNeverRefreshes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex-auth.json")
	var calls atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer issuer.Close()
	s := newCodexAuthStore()
	s.path, s.issuer, s.client = path, issuer.URL, issuer.Client()
	s.life = newLifecycle(false)
	if err := s.life.quiesce(); err != nil {
		t.Fatal(err)
	}
	c := usageCredential("account")
	c.Tokens.AccessToken = testJWT(time.Now().Add(-time.Minute))
	data, _ := json.Marshal(c)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.credentialFor(t.Context()); err == nil {
		t.Fatal("quiesced instance refreshed expired token")
	}
	c.Tokens.AccessToken = testJWT(time.Now().Add(time.Hour))
	data, _ = json.Marshal(c)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.credentialFor(t.Context())
	if err != nil || got.Tokens.AccessToken != c.Tokens.AccessToken || calls.Load() != 0 {
		t.Fatalf("quiesced reread failed: %v, refresh calls %d", err, calls.Load())
	}
}
