package config

import (
	"fmt"
	"net/url"
	"strings"
)

// CheckUpstream keeps client credentials inside Anthropic: the Anthropic route
// forwards the client's Authorization and x-api-key to this URL, so it must be
// https on api.anthropic.com or another *.anthropic.com host. allowAny lifts the
// check for local test stubs and says so in the log.
func CheckUpstream(u *url.URL, allowAny bool, logf func(string, ...any)) error {
	if allowAny {
		logf("ROUTER_UPSTREAM_ALLOW_ANY=1: upstream host check is off, upstream %s", u.Redacted())
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme == "https" && u.User == nil && u.Opaque == "" &&
		(host == "api.anthropic.com" || strings.HasSuffix(host, ".anthropic.com")) {
		return nil
	}
	return fmt.Errorf("ROUTER_UPSTREAM_URL must be https://api.anthropic.com or another https://*.anthropic.com host, got %q; set ROUTER_UPSTREAM_ALLOW_ANY=1 to allow any upstream", u.Redacted())
}
