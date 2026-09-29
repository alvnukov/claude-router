package main

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// errUpstreamIdle is what a watched body returns once the upstream has been
// silent past its bound, whichever phase it was in.
var errUpstreamIdle = errors.New("upstream idle")

// idleError names the phase and the bound that passed: "upstream idle 300s",
// "upstream start 30s".
type idleError struct {
	phase string
	bound time.Duration
}

func (e *idleError) Error() string {
	bound := e.bound.String()
	if e.bound%time.Second == 0 {
		bound = fmt.Sprintf("%ds", e.bound/time.Second)
	}
	return "upstream " + e.phase + " " + bound
}

func (e *idleError) Unwrap() error { return errUpstreamIdle }

// idleMidAnswer reports a silence after the upstream had started sending:
// the model was working, so another member would likely be as slow.
func idleMidAnswer(err error) bool {
	var idle *idleError
	return errors.As(err, &idle) && idle.phase == "idle"
}

type bodyWatch struct {
	body  io.ReadCloser
	idle  time.Duration
	abort func(error)

	mu     sync.Mutex
	timer  *time.Timer
	gen    int // bumped on every re-arm, so a stale timer does nothing
	closed bool
	last   time.Time
	maxGap time.Duration
	err    error
}

// watchBody wraps an upstream response body. start bounds the wait for the
// first byte after the headers, idle bounds every later pause; 0 disables a
// phase. When a bound passes, abort is called once with the reason and every
// following Read returns it. MaxGap is the longest pause seen between reads.
func watchBody(body io.ReadCloser, start, idle time.Duration, abort func(error)) *bodyWatch {
	b := &bodyWatch{body: body, idle: idle, abort: abort, last: time.Now()}
	b.mu.Lock()
	b.arm("start", start)
	b.mu.Unlock()
	return b
}

// arm replaces the running bound; the caller holds mu.
func (b *bodyWatch) arm(phase string, bound time.Duration) {
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.gen++
	if bound <= 0 {
		return
	}
	gen := b.gen
	b.timer = time.AfterFunc(bound, func() { b.expire(gen, &idleError{phase, bound}) })
}

func (b *bodyWatch) expire(gen int, err *idleError) {
	b.mu.Lock()
	if gen != b.gen || b.closed || b.err != nil {
		b.mu.Unlock()
		return
	}
	b.err = err
	b.maxGap = max(b.maxGap, err.bound)
	b.mu.Unlock()
	b.abort(err)
}

func (b *bodyWatch) Read(p []byte) (int, error) {
	b.mu.Lock()
	err := b.err
	b.mu.Unlock()
	if err != nil {
		return 0, err
	}
	n, err := b.body.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return n, b.err
	}
	if n > 0 {
		now := time.Now()
		b.maxGap = max(b.maxGap, now.Sub(b.last))
		b.last = now
		if !b.closed {
			b.arm("idle", b.idle)
		}
	}
	return n, err
}

// Close stops the timer and closes the body.
func (b *bodyWatch) Close() error {
	b.mu.Lock()
	b.closed = true
	b.arm("", 0)
	b.mu.Unlock()
	return b.body.Close()
}

func (b *bodyWatch) MaxGap() time.Duration {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.maxGap
}
