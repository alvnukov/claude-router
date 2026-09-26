package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"
)

// The checker pings every configured model that real traffic has not reached
// in the last interval, all of them in parallel, so ratings and cooldowns
// stay current for the models balancing is not sending anything to. A probe
// is a one-token completion; it moves the rating, the latency and the
// cooldown like a real request but is counted apart from real traffic.

func startChecker(ctx context.Context, cs *configStore, hl *health, life *lifecycle) {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		running := map[string]bool{}
		done := make(chan string)
		for {
			if life.mode() == modeActive && ctx.Err() == nil {
				for key, pc := range poolProbeConfigs(cs.Get()) {
					if running[key] {
						continue
					}
					last := hl.snapshot(key).LastAt
					if !last.IsZero() && time.Since(last) < pc.ProbeEvery {
						continue
					}
					running[key] = true
					go func() {
						checkModels(pc, hl)
						select {
						case done <- key:
						case <-ctx.Done():
						}
					}()
				}
			}
			select {
			case key := <-done:
				delete(running, key)
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Probe only pool members. Shared members are checked once, using the shortest
// enabled interval (and that pool's timeout). A disabled pool schedules nothing.
func poolProbeConfigs(c config) map[string]config {
	out := map[string]config{}
	names := make([]string, 0, len(c.Local.ModelPools))
	for name := range c.Local.ModelPools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pc := c.PoolSettings(name).Apply(c)
		if pc.ProbeEvery <= 0 {
			continue
		}
		for _, target := range c.Local.ModelPools[name] {
			if previous, ok := out[target.Model]; ok && previous.ProbeEvery <= pc.ProbeEvery {
				continue
			}
			for _, model := range c.Local.Models {
				if model.Key() != target.Model {
					continue
				}
				p, ok := c.Local.Provider(model.Provider)
				if !ok || p.Type == "codex" {
					break
				}
				next := pc
				next.Local = localSetup{Providers: []provider{p}, Models: []localModel{model}}
				out[target.Model] = next
				break
			}
		}
	}
	return out
}

func checkModels(c config, hl *health) {
	c.Failover = true // pick lists every model only with failover on
	if c.FirstByte <= 0 || c.FirstByte > c.ProbeEvery {
		c.FirstByte = c.ProbeEvery
	}
	var wg sync.WaitGroup
	for _, cand := range hl.pick(c) {
		// Subscription quota must not be spent by background health checks.
		if cand.Provider.Type == "codex" {
			continue
		}
		if !cand.Stat.LastAt.IsZero() && time.Since(cand.Stat.LastAt) < c.ProbeEvery {
			continue
		}
		wg.Add(1)
		go func(cand candidate) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*c.ProbeEvery)
			defer cancel()
			r, _ := http.NewRequestWithContext(ctx, "POST", "/", nil)
			payload, _ := json.Marshal(openaiRequest{
				Model:     cand.Model,
				Messages:  []openaiMsg{{Role: "user", Content: "ping"}},
				MaxTokens: 1,
			})
			res := tryModel(r, c, cand, payload, false)
			if res.err != nil {
				log.Printf("check %s: %v", cand.Key, res.err)
				hl.recordProbe(cand.Key, false, 0, "проверка: "+res.err.Error())
				return
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(res.resp.Body, 1<<16)) // drain for reuse; the probe has its answer
			res.resp.Body.Close()
			res.cancel()
			hl.recordProbe(cand.Key, true, res.ttfb, "")
		}(cand)
	}
	wg.Wait()
}
