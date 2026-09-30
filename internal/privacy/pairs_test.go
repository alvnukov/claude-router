package privacy

import (
	"testing"
	"time"
)

func TestPairStore(t *testing.T) {
	start := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	want := func(t *testing.T, p *pairStore, session, pseudo, real string, now time.Time) {
		t.Helper()
		got, kind, ok := p.get(session, pseudo, now)
		if !ok || got != real || kind != KindEmail {
			t.Fatalf("get(%s, %s) = %q, %q, %v; want %q, %q, true", session, pseudo, got, kind, ok, real, KindEmail)
		}
	}
	gone := func(t *testing.T, p *pairStore, session, pseudo string, now time.Time) {
		t.Helper()
		if got, _, ok := p.get(session, pseudo, now); ok {
			t.Fatalf("get(%s, %s) = %q; want no pair", session, pseudo, got)
		}
	}

	t.Run("same pair twice", func(t *testing.T) {
		p := newPairStore()
		if p.put("s", "p1", "r1", KindEmail, start) || p.put("s", "p1", "r1", KindEmail, start) {
			t.Fatal("the same pair reported a collision")
		}
		want(t, p, "s", "p1", "r1", start)
	})
	t.Run("collision keeps first", func(t *testing.T) {
		p := newPairStore()
		p.put("s", "p1", "r1", KindEmail, start)
		if !p.put("s", "p1", "r2", KindEmail, start) {
			t.Fatal("a second value under one pseudonym was not a collision")
		}
		want(t, p, "s", "p1", "r1", start)
	})
	t.Run("limit evicts least recently used", func(t *testing.T) {
		p := newPairStore()
		p.limit = 3
		p.put("s", "p1", "r1", KindEmail, start)
		p.put("s", "p2", "r2", KindEmail, start.Add(time.Second))
		p.put("s", "p3", "r3", KindEmail, start.Add(2*time.Second))
		want(t, p, "s", "p1", "r1", start.Add(3*time.Second))
		p.put("s", "p4", "r4", KindEmail, start.Add(4*time.Second))
		now := start.Add(5 * time.Second)
		want(t, p, "s", "p1", "r1", now)
		gone(t, p, "s", "p2", now)
		want(t, p, "s", "p3", "r3", now)
		want(t, p, "s", "p4", "r4", now)
	})
	t.Run("ttl counts from last access", func(t *testing.T) {
		p := newPairStore()
		p.put("s", "p1", "r1", KindEmail, start)
		for h := 1; h <= 30; h++ {
			want(t, p, "s", "p1", "r1", start.Add(time.Duration(h)*time.Hour))
		}
	})
	t.Run("idle pair expires", func(t *testing.T) {
		p := newPairStore()
		p.put("s", "p1", "r1", KindEmail, start)
		gone(t, p, "s", "p1", start.Add(pairTTL+time.Second))
	})
	t.Run("idle session swept by another session", func(t *testing.T) {
		p := newPairStore()
		p.put("a", "p1", "r1", KindEmail, start)
		p.put("b", "p2", "r2", KindEmail, start.Add(25*time.Hour))
		p.mu.Lock()
		_, kept := p.sessions["a"]
		p.mu.Unlock()
		if kept {
			t.Fatal("a session idle for 25 h outlived another session's put")
		}
	})
	t.Run("sessions isolated", func(t *testing.T) {
		p := newPairStore()
		p.put("a", "p1", "r1", KindEmail, start)
		if p.put("b", "p1", "r2", KindEmail, start) {
			t.Fatal("one pseudonym in two sessions was a collision")
		}
		want(t, p, "a", "p1", "r1", start)
		want(t, p, "b", "p1", "r2", start)
	})
	t.Run("forget", func(t *testing.T) {
		p := newPairStore()
		p.put("a", "p1", "r1", KindEmail, start)
		p.put("b", "p2", "r2", KindEmail, start)
		p.forget("a")
		gone(t, p, "a", "p1", start)
		want(t, p, "b", "p2", "r2", start)
		p.forget("")
		gone(t, p, "b", "p2", start)
	})
	t.Run("shared per directory", func(t *testing.T) {
		dir := t.TempDir()
		if sharedPairs(dir) != sharedPairs(dir) || sharedPairs(dir) == sharedPairs(t.TempDir()) {
			t.Fatal("pair stores are not one per directory")
		}
	})
}
