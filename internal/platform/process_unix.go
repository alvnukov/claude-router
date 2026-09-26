//go:build unix

package platform

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// StartDetached starts exe with args in a session of its own, appending its
// output to logPath, and returns its pid without waiting for it. The caller's
// terminal and its interrupts no longer reach the process.
func StartDetached(exe string, args []string, logPath string) (int, error) {
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer log.Close()
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	return pid, cmd.Process.Release()
}

// Terminate asks the process pid to stop, as the service manager does.
func Terminate(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// KillMatching terminates every process whose command line matches the
// extended regular expression pattern, except the caller. macOS pgrep already
// leaves out its ancestors; procps pgrep leaves out only itself.
func KillMatching(pattern string) error {
	out, err := exec.Command("pgrep", "-f", pattern).Output()
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil // nothing matched
	}
	if err != nil {
		return err
	}
	for _, field := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(field)
		if err != nil || pid == os.Getpid() {
			continue
		}
		if err := Terminate(pid); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	return nil
}
