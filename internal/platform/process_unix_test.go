//go:build unix

package platform

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// blockChild is the body of a child process the tests start from their own
// binary: it says it is ready and waits to be killed.
func blockChild(t *testing.T, flag string) bool {
	t.Helper()
	if os.Getenv(flag) != "1" {
		return false
	}
	os.Stdout.WriteString("child ready\n")
	<-time.After(30 * time.Second)
	return true
}

// waitSignaled reaps pid and fails unless SIGTERM ended it.
func waitSignaled(t *testing.T, pid int) {
	t.Helper()
	var status syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &status, 0, nil); err != nil {
		t.Fatal(err)
	}
	if !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Fatalf("child ended with %v; want killed by SIGTERM", status)
	}
}

func TestStartDetachedLogsAndLeavesTheCallersGroup(t *testing.T) {
	if blockChild(t, "PLATFORM_DETACHED_CHILD") {
		return
	}
	t.Setenv("PLATFORM_DETACHED_CHILD", "1")
	logPath := filepath.Join(t.TempDir(), "router.log")
	if err := os.WriteFile(logPath, []byte("earlier\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pid, err := StartDetached(os.Args[0], []string{"-test.run=^TestStartDetachedLogsAndLeavesTheCallersGroup$"}, logPath)
	if err != nil {
		t.Fatal(err)
	}
	deadline, tick := time.After(10*time.Second), time.NewTicker(10*time.Millisecond)
	defer tick.Stop()
	for {
		data, _ := os.ReadFile(logPath)
		if strings.HasPrefix(string(data), "earlier\n") && strings.Contains(string(data), "child ready\n") {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("log never got the child's output: %q", data)
		case <-tick.C:
		}
	}
	// A new session: an interrupt at the caller's terminal does not reach it.
	if group, err := syscall.Getpgid(pid); err != nil || group != pid {
		t.Fatalf("child group %d, %v; want its own group %d", group, err, pid)
	}
	if err := Terminate(pid); err != nil {
		t.Fatal(err)
	}
	waitSignaled(t, pid)
}

// The router's own command line matches the pattern that finds a hand-started
// router, so the caller must not kill itself.
func TestKillMatchingSparesTheCaller(t *testing.T) {
	if blockChild(t, "PLATFORM_KILL_CHILD") {
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestKillMatchingSparesTheCaller$")
	cmd.Env = append(os.Environ(), "PLATFORM_KILL_CHILD=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "child ready\n" {
		t.Fatalf("child did not start: %q %v", line, err)
	}
	if err := KillMatching(regexp.QuoteMeta(os.Args[0]) + "( |$)"); err != nil {
		t.Fatal(err)
	}
	waitSignaled(t, cmd.Process.Pid)
}

func TestKillMatchingWithoutMatchIsNoError(t *testing.T) {
	if err := KillMatching("no-such-process-" + strings.Repeat("x", 20) + "( |$)"); err != nil {
		t.Fatal(err)
	}
}
