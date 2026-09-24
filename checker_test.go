package main

import (
	"strings"
	"testing"
	"time"
)

func keys(cs []candidate) string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Key)
	}
	return strings.Join(out, ",")
}

// The head goes to the least loaded of the best models; the tail stays in
// rating order so a failure moves to the best-rated one.
func TestPickBalances(t *testing.T) {
	hl := newHealth("")
	cfg := config{local: oneProvider("http://h/v1", "a", "b", "c", "d"), failover: true, balance: 3}
	for key, score := range map[string]float64{"p/a": 1, "p/b": 0.9, "p/c": 0.8, "p/d": 0.7} {
		hl.stat(key).Score = score
	}
	if got := keys(hl.pick(cfg)); got != "p/a,p/b,p/c,p/d" {
		t.Fatalf("idle: %s", got)
	}
	hl.acquire("p/a")
	if got := keys(hl.pick(cfg)); got != "p/b,p/a,p/c,p/d" {
		t.Fatalf("a busy: %s", got)
	}
	hl.acquire("p/b")
	hl.acquire("p/c")
	hl.acquire("p/c")
	if got := keys(hl.pick(cfg)); got != "p/a,p/b,p/c,p/d" {
		t.Fatalf("all busy, tie keeps rating order and d is outside the group: %s", got)
	}
	hl.stat("p/b").CoolUntil = time.Now().Add(time.Minute)
	hl.release("p/a")
	hl.acquire("p/a")
	hl.acquire("p/a")
	if got := keys(hl.pick(cfg)); got != "p/d,p/a,p/c,p/b" {
		t.Fatalf("cooling b leaves the group, idle d takes its place: %s", got)
	}
	cfg.balance = 1
	if got := keys(hl.pick(cfg)); got != "p/a,p/c,p/d,p/b" {
		t.Fatalf("balance off: %s", got)
	}
	hl.release("p/a")
	hl.release("p/a")
	hl.release("p/a") // one more than acquired: must not go negative
	if hl.load("p/a") != 0 {
		t.Fatal("release went negative")
	}
}

// Probes move the rating and the cooldown but not the ok/fail counters, skip
// models that had traffic in the last half interval, and run in parallel.
func TestCheckModels(t *testing.T) {
	var calls testCalls
	srv := fakeEndpoint(t, &calls)
	defer srv.Close()
	hl := newHealth("")
	hl.record("p/good", true, time.Second, "") // just used: not probed
	cfg := config{local: oneProvider(srv.URL, "good", "bad", "slow", "other"), probeEvery: 10 * time.Second, firstByte: time.Second}
	t0 := time.Now()
	checkModels(cfg, hl)
	if took := time.Since(t0); took > 1400*time.Millisecond {
		t.Fatalf("probes ran one after another: %s", took)
	}
	if strings.Contains(strings.Join(calls.snapshot(), ","), "good") || len(calls.snapshot()) != 3 {
		t.Fatalf("probed %v", calls.snapshot())
	}
	bad, other, slow := hl.snapshot("p/bad"), hl.snapshot("p/other"), hl.snapshot("p/slow")
	if bad.ProbeFail != 1 || bad.Fail != 0 || !bad.Cooling() || bad.Score >= 1 || !strings.Contains(bad.LastErr, "проверка") {
		t.Fatalf("bad: %+v", bad)
	}
	if other.ProbeOK != 1 || other.OK != 0 || other.TTFBMs == 0 || other.ProbeAt.IsZero() {
		t.Fatalf("other: %+v", other)
	}
	if slow.ProbeFail != 1 || !strings.Contains(slow.LastErr, "no response within") {
		t.Fatalf("slow: %+v", slow)
	}
	hl.stat("p/bad").CoolUntil = time.Time{}
	checkModels(cfg, hl)
	if len(calls.snapshot()) != 3 {
		t.Fatalf("just-probed models were probed again: %v", calls.snapshot())
	}
}

// A request releases its in-flight slot on every exit path.
func TestInFlightReleased(t *testing.T) {
	var calls testCalls
	srv := fakeEndpoint(t, &calls)
	defer srv.Close()
	hl := newHealth("")
	cfg := config{local: oneProvider(srv.URL, "bad", "good"), failover: true, firstByte: 5 * time.Second}
	if w, _ := runLocal(t, cfg, hl); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if hl.load("p/bad") != 0 || hl.load("p/good") != 0 {
		t.Fatalf("in flight after the request: bad=%d good=%d", hl.load("p/bad"), hl.load("p/good"))
	}
}
