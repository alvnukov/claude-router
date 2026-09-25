//go:build unix

package platform

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// waitDone fails the test unless ctx ends within a generous bound.
func waitDone(t *testing.T, ctx context.Context, what string) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("context not done after %s", what)
	}
}

func TestShutdownContextEndsOnSIGTERM(t *testing.T) {
	ctx, stop := ShutdownContext(t.Context(), "com.example.svc")
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitDone(t, ctx, "SIGTERM")
}

func TestShutdownContextEndsWithParent(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	ctx, stop := ShutdownContext(parent, "com.example.svc")
	defer stop()
	cancel()
	waitDone(t, ctx, "the parent's cancel")
}

// After stop the process no longer catches SIGTERM, so the signal ends it as
// it would without the router's handler. The check runs in a child process.
func TestShutdownContextStopRestoresSIGTERM(t *testing.T) {
	if os.Getenv("PLATFORM_SHUTDOWN_CHILD") == "1" {
		_, stop := ShutdownContext(context.Background(), "com.example.svc")
		stop()
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		<-time.After(10 * time.Second)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestShutdownContextStopRestoresSIGTERM$")
	cmd.Env = append(os.Environ(), "PLATFORM_SHUTDOWN_CHILD=1")
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("child survived SIGTERM after stop: %v", err)
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Fatalf("child ended with %v; want killed by SIGTERM", err)
	}
}
