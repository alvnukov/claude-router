package platform

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

// The installed legacy agent was written by the shell script; an update that
// renders it from Go must leave it unchanged.
func TestLaunchdPlistMatchesLegacyAgent(t *testing.T) {
	home := "/home/router/.claude/local-router"
	spec := ServiceSpec{Label: "com.claude-local-router", Exe: home + "/localrouter", Dir: home, LogPath: home + "/router.log", KeepAlive: true, ThrottleInterval: 10 * time.Second}
	want, err := os.ReadFile("testdata/launchd-com.claude-local-router.plist")
	if err != nil {
		t.Fatal(err)
	}
	if got := LaunchdPlist(spec); !bytes.Equal(got, want) {
		t.Fatalf("legacy plist differs:\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func TestLaunchdPlistWritesOptionalKeysOnlyWhenSet(t *testing.T) {
	spec := ServiceSpec{Label: "l", Exe: "/bin/x", Args: []string{"a&b"}, Dir: "/d", LogPath: "/d/log", Env: map[string]string{"B": "2", "A": "<1>"}, ExitTimeout: 960 * time.Second}
	got := string(LaunchdPlist(spec))
	for _, want := range []string{
		"<array><string>/bin/x</string><string>a&amp;b</string></array>",
		"<key>KeepAlive</key><false/>",
		"<key>ExitTimeOut</key><integer>960</integer>",
		"<key>A</key><string>&lt;1&gt;</string>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("plist lacks %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ThrottleInterval") {
		t.Fatalf("zero ThrottleInterval was written:\n%s", got)
	}
	if strings.Index(got, "<key>A</key>") > strings.Index(got, "<key>B</key>") {
		t.Fatalf("environment is not sorted:\n%s", got)
	}
	bare := string(LaunchdPlist(ServiceSpec{Label: "l", Exe: "/bin/x"}))
	for _, absent := range []string{"ExitTimeOut", "EnvironmentVariables"} {
		if strings.Contains(bare, absent) {
			t.Fatalf("unset %s was written:\n%s", absent, bare)
		}
	}
}
