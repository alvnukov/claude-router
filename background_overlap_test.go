package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	conf "localrouter/internal/config"
)

func TestCheckerStopsOutgoingProbesAfterQuiesce(t *testing.T) {
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer endpoint.Close()
	c := config{Local: localSetup{
		Providers:    []provider{{Name: "test", Type: "openai", BaseURL: endpoint.URL}},
		Models:       []localModel{{Provider: "test", Model: "m"}},
		ModelPools:   map[string][]poolTarget{"pool": {{Model: "test/m"}}},
		PoolSettings: map[string]poolSettings{"pool": {ProbeSec: 1, FirstByteSec: 1}},
	}}
	life := newLifecycle(false)
	h := newHealth("")
	h.life = life
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startChecker(ctx, conf.NewStore(c, ""), h, life)
	deadline := time.After(3 * time.Second)
	for calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("checker did not issue initial probe")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := life.quiesce(); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	time.Sleep(1300 * time.Millisecond)
	if calls.Load() != before {
		t.Fatalf("quiesced checker issued %d new probes", calls.Load()-before)
	}
}
