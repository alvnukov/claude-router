package main

import (
	"testing"

	"localrouter/internal/platform"
)

// goldenCutoverOps renders with fixed paths and addresses, so the golden
// files do not depend on the machine or on free ports.
func goldenCutoverOps() *systemCutoverOps {
	home := "/home/router/.claude/local-router"
	cfg := deployConfig{PublicAPI: "127.0.0.1:18787", PublicUI: "127.0.0.1:18788", BlueAPI: "127.0.0.1:18791", BlueUI: "127.0.0.1:18793", GreenAPI: "127.0.0.1:18792", GreenUI: "127.0.0.1:18794"}
	return newSystemCutoverOps(deployFile{CaddyAdmin: "127.0.0.1:12019", deployConfig: cfg}, home, "/home/router/Library/LaunchAgents", home+"/localrouter.candidate", "/opt/homebrew/bin/caddy")
}

// The slot and Caddy agents a cutover already installed must stay byte for
// byte what the next deploy writes.
func TestSlotAndCaddyPlistsMatchGolden(t *testing.T) {
	skipDarwinOnlyDeploy(t)
	ops := goldenCutoverOps()
	assertGolden(t, "testdata/launchd-com.claude-local-router.green.plist", platform.LaunchdPlist(ops.slotSpec("green")))
	assertGolden(t, "testdata/launchd-com.claude-local-router.caddy.plist", platform.LaunchdPlist(ops.caddySpec()))
}
