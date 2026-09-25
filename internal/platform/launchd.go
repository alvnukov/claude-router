package platform

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"sort"
	"time"
)

// LaunchdPlist renders spec as a launchd agent definition. The agent starts
// at load and runs as a background process; keys whose value is unset are
// left out so launchd applies its defaults.
func LaunchdPlist(spec ServiceSpec) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	str := func(key, value string) {
		fmt.Fprintf(&b, "  <key>%s</key><string>%s</string>\n", key, plistEscape(value))
	}
	boolean := func(key string, value bool) {
		fmt.Fprintf(&b, "  <key>%s</key><%t/>\n", key, value)
	}
	seconds := func(key string, d time.Duration) {
		if s := int64(d / time.Second); s > 0 {
			fmt.Fprintf(&b, "  <key>%s</key><integer>%d</integer>\n", key, s)
		}
	}
	str("Label", spec.Label)
	b.WriteString("  <key>ProgramArguments</key>\n  <array>")
	for _, arg := range append([]string{spec.Exe}, spec.Args...) {
		fmt.Fprintf(&b, "<string>%s</string>", plistEscape(arg))
	}
	b.WriteString("</array>\n")
	str("WorkingDirectory", spec.Dir)
	boolean("RunAtLoad", true)
	boolean("KeepAlive", spec.KeepAlive)
	seconds("ThrottleInterval", spec.ThrottleInterval)
	seconds("ExitTimeOut", spec.ExitTimeout)
	str("ProcessType", "Background")
	if len(spec.Env) > 0 {
		b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		keys := make([]string, 0, len(spec.Env))
		for key := range spec.Env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(&b, "    <key>%s</key><string>%s</string>\n", plistEscape(key), plistEscape(spec.Env[key]))
		}
		b.WriteString("  </dict>\n")
	}
	str("StandardOutPath", spec.LogPath)
	str("StandardErrorPath", spec.LogPath)
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

func plistEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s)) // a bytes.Buffer does not fail
	return b.String()
}
