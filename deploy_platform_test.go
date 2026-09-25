package main

import (
	"runtime"
	"testing"
)

// skipDarwinOnlyDeploy skips a test that drives the zero-downtime deploy
// through launchd, Caddy or bash: Windows has no counterpart for them.
func skipDarwinOnlyDeploy(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("zero-downtime deploy is darwin-only")
	}
}
