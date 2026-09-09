package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// The checker pings every configured model that real traffic has not reached
// in the last half interval, all of them in parallel, so ratings and cooldowns
// stay current for the models balancing is not sending anything to. A probe
// is a one-token completion; it moves the rating, the latency and the
// cooldown like a real request but is counted apart from real traffic.

func startChecker(cs *configStore, hl *health) {
	go func() {
		for {
			c := cs.get()
			if c.probeEvery <= 0 {
				time.Sleep(5 * time.Second)
				continue
			}
			checkModels(c, hl)
			time.Sleep(c.probeEvery)
		}
	}()
}

func checkModels(c config, hl *health) {
	c.failover = true // pick lists every model only with failover on
	if c.firstByte <= 0 || c.firstByte > c.probeEvery {
		c.firstByte = c.probeEvery
	}
	var wg sync.WaitGroup
	for _, cand := range hl.pick(c) {
		if !cand.Stat.LastAt.IsZero() && time.Since(cand.Stat.LastAt) < c.probeEvery/2 {
			continue
		}
		wg.Add(1)
		go func(cand candidate) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*c.probeEvery)
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
			io.Copy(io.Discard, io.LimitReader(res.resp.Body, 1<<16))
			res.resp.Body.Close()
			res.cancel()
			hl.recordProbe(cand.Key, true, res.ttfb, "")
		}(cand)
	}
	wg.Wait()
}
