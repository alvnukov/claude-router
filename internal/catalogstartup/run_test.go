package catalogstartup

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestStartRefreshesImmediatelyAndJoinsAfterCancel(t *testing.T) {
	deps := Production("unused", "unused")
	defer deps.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	var calls atomic.Int32
	run := deps.Start(ctx, time.Hour, func(ctx context.Context) error {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("initial refresh did not start immediately")
	}
	cancel()
	wait, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := deps.Wait(wait, run.Done); err != nil {
		t.Fatalf("join: %v", err)
	}
	select {
	case <-run.Ready:
	default:
		t.Fatal("initial refresh did not signal completion")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want one immediate refresh", got)
	}
}

func TestStartRepeatsOnIntervalUntilCanceled(t *testing.T) {
	deps := Production("unused", "unused")
	defer deps.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := make(chan struct{}, 2)
	run := deps.Start(ctx, 10*time.Millisecond, func(context.Context) error {
		select {
		case called <- struct{}{}:
		default:
		}
		return nil
	})
	select {
	case <-run.Ready:
	case <-time.After(time.Second):
		t.Fatal("initial refresh did not finish")
	}
	<-called
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("no periodic refresh")
	}
	cancel()
	wait, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := deps.Wait(wait, run.Done); err != nil {
		t.Fatalf("join: %v", err)
	}
}

func TestWaitHonorsShutdownDeadline(t *testing.T) {
	deps := Production("unused", "unused")
	defer deps.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	run := deps.Start(ctx, time.Hour, func(context.Context) error {
		close(entered)
		<-release
		return nil
	})
	<-entered
	wait, stop := context.WithCancel(context.Background())
	stop()
	if err := deps.Wait(wait, run.Done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait canceled context = %v", err)
	}
	cancel()
	close(release)
	joined, end := context.WithTimeout(context.Background(), time.Second)
	defer end()
	if err := deps.Wait(joined, run.Done); err != nil {
		t.Fatalf("join after release: %v", err)
	}
}
