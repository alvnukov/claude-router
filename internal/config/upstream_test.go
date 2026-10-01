package config

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestCheckUpstream(t *testing.T) {
	cases := []struct {
		name, raw string
		allowAny  bool
		ok        bool
	}{
		{"default", "https://api.anthropic.com", false, true},
		{"host case", "https://API.Anthropic.com", false, true},
		{"subdomain with port and path", "https://eu.api.anthropic.com:443/v1", false, true},
		{"plain http", "http://api.anthropic.com", false, false},
		{"foreign host", "https://evil.example", false, false},
		{"anthropic prefix on a foreign host", "https://api.anthropic.com.evil.com", false, false},
		{"suffix without a dot", "https://evilanthropic.com", false, false},
		{"bare anthropic.com", "https://anthropic.com", false, false},
		{"trailing dot", "https://api.anthropic.com.", false, false},
		{"userinfo", "https://user:pw@api.anthropic.com", false, false},
		{"empty host", "https://", false, false},
		{"no scheme", "api.anthropic.com", false, false},
		{"opaque", "https:api.anthropic.com", false, false},
		// U+0130 lowers to an ASCII i, but the transport dials its punycode.
		{"non-ASCII letter that lowers to ASCII", "https://api.anthropİc.com", false, false},
		{"IPv6 zone ending in anthropic.com", "https://[::1%25x.anthropic.com]", false, false},
		{"bypass admits a local stub", "http://127.0.0.1:1", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, err := url.Parse(c.raw)
			if err != nil {
				t.Fatal(err)
			}
			var logs []string
			logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
			err = CheckUpstream(u, c.allowAny, logf)
			if (err == nil) != c.ok {
				t.Fatalf("CheckUpstream(%q) = %v, want ok=%v", c.raw, err, c.ok)
			}
			if err != nil && !strings.Contains(err.Error(), "ROUTER_UPSTREAM_ALLOW_ANY=1") {
				t.Fatalf("refusal does not name the bypass: %v", err)
			}
			if !c.allowAny && len(logs) != 0 {
				t.Fatalf("logged without the bypass: %q", logs)
			}
			if c.allowAny && (len(logs) != 1 || !strings.Contains(logs[0], "ROUTER_UPSTREAM_ALLOW_ANY=1") || !strings.Contains(logs[0], "127.0.0.1:1")) {
				t.Fatalf("bypass log = %q, want one line naming the bypass and the host", logs)
			}
		})
	}
	t.Run("bypass log and refusal mask the password", func(t *testing.T) {
		u, _ := url.Parse("http://user:secretpw@127.0.0.1:1")
		var logs []string
		if err := CheckUpstream(u, true, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }); err != nil {
			t.Fatal(err)
		}
		if len(logs) != 1 || strings.Contains(logs[0], "secretpw") || !strings.Contains(logs[0], "127.0.0.1:1") {
			t.Fatalf("bypass log = %q", logs)
		}
		if err := CheckUpstream(u, false, func(string, ...any) {}); err == nil || strings.Contains(err.Error(), "secretpw") {
			t.Fatalf("refusal = %v", err)
		}
	})
}
