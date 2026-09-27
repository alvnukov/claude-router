package catalogstartup

import (
	"context"
	"log"
	"time"
)

// Run exposes the first refresh's completion separately from the lifetime of
// the hourly loop. Ready says nothing about whether the refresh succeeded.
type Run struct {
	Ready <-chan struct{}
	Done  <-chan struct{}
}

func (d Dependencies) Start(ctx context.Context, interval time.Duration, refresh func(context.Context) error) Run {
	ready, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		if ctx.Err() == nil {
			if err := refresh(ctx); err != nil {
				log.Printf("model catalog refresh: %v", err)
			}
		}
		close(ready)
		if ctx.Err() != nil {
			return
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if ctx.Err() != nil {
					return
				}
				if err := refresh(ctx); err != nil {
					log.Printf("model catalog refresh: %v", err)
				}
			}
		}
	}()
	return Run{Ready: ready, Done: done}
}

func (d Dependencies) Wait(ctx context.Context, done <-chan struct{}) error {
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
