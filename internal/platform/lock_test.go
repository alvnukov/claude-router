package platform

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lockHelperEnv makes the test binary a helper process that holds the lock
// named by the variable until its stdin closes.
const lockHelperEnv = "PLATFORM_TEST_HOLD_LOCK"

func TestMain(m *testing.M) {
	if path := os.Getenv(lockHelperEnv); path != "" {
		err := WithLock(context.Background(), path, func() error {
			fmt.Println("held")
			_, err := io.Copy(io.Discard, os.Stdin)
			return err
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// holdLock holds the lock at path from another goroutine until the test ends.
func holdLock(t *testing.T, path string) {
	t.Helper()
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error)
	go func() {
		done <- WithLock(context.Background(), path, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("hold lock: %v", err)
	}
	t.Cleanup(func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
}

func mustNotRun(t *testing.T) func() error {
	return func() error {
		t.Error("ran while another holder had the lock")
		return nil
	}
}

func TestWithLockRunsFnAndReturnsItsError(t *testing.T) {
	want := errors.New("from fn")
	if err := WithLock(t.Context(), filepath.Join(t.TempDir(), "a.lock"), func() error { return want }); err != want {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestWithLockCreatesMissingDirectories(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	ran := false
	if err := WithLock(t.Context(), filepath.Join(dir, "x.lock"), func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("directory: %v %v", fi, err)
	}
}

func TestWithLockReportsUnopenableFile(t *testing.T) {
	dir := t.TempDir()
	err := WithLock(t.Context(), dir, mustNotRun(t))
	if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("lock on a directory: %v", err)
	}
}

func TestWithLockWaitsUntilContextEnds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	holdLock(t, path)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := WithLock(ctx, path, mustNotRun(t)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

// A context that is done already asks for exactly one try: a free lock is
// taken, a held one fails at once.
func TestWithLockTriesOnceWithDoneContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	done, cancel := context.WithCancel(t.Context())
	cancel()
	ran := false
	if err := WithLock(done, path, func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("free lock: ran=%v err=%v", ran, err)
	}
	holdLock(t, path)
	if err := WithLock(done, path, mustNotRun(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("held lock: err = %v, want canceled", err)
	}
}

func TestWithLockSerializesHolders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	var inside, overlaps atomic.Int32
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 10 {
				err := WithLock(t.Context(), path, func() error {
					if inside.Add(1) != 1 {
						overlaps.Add(1)
					}
					runtime.Gosched()
					inside.Add(-1)
					return nil
				})
				if err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	if n := overlaps.Load(); n != 0 {
		t.Fatalf("%d holders overlapped", n)
	}
}

func TestWithLockExcludesOtherProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), lockHelperEnv+"="+path)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// On a failed check the helper would hold the lock file open, and Windows
	// could not remove the test's directory.
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "held\n" {
		t.Fatalf("helper: %q %v", line, err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := WithLock(ctx, path, mustNotRun(t)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("while the helper holds it: err = %v, want deadline exceeded", err)
	}

	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper: %v", err)
	}
	ran := false
	if err := WithLock(t.Context(), path, func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("after the helper exits: ran=%v err=%v", ran, err)
	}
}
