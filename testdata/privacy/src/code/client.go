package billing

import (
	"net/http"
	"net/netip"
	"time"
)

// Partner API of ⟦org:Vasilek⟧; the staging copy lives at ⟦host:partner-api.staging.vasilek.example⟧.
const (
	partnerURL   = "https://⟦host:partner-api.vasilek.example⟧/v2"
	partnerToken = "⟦secret:ghp_FAKEr0mashkaT0kenD0N0tUseXXXXXXXXXXX⟧" // TODO: move to vault
	slackHook    = "https://hooks.slack.com/services/⟦secret:FAKE/FAKE/INVALID⟧"
)

var trusted = []netip.Prefix{
	netip.MustParsePrefix("⟦cidr4:10.113.0.0/16⟧"),
	netip.MustParsePrefix("⟦cidr6:2001:db8:4a1::/48⟧"),
	netip.MustParsePrefix("⟦keep:127.0.0.0/8⟧"),
}

var fallbackResolver = netip.MustParseAddrPort("⟦ipv4:10.113.0.53⟧:53")

func newClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}

func authHeader(req *http.Request) {
	req.Header.Set("Authorization", "Bearer ⟦secret:eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJyb21hc2hrYS10ZXN0In0.RkFLRXNpZ25hdHVyZUZBS0VGQUtF⟧")
	req.Header.Set("X-Client", "⟦org:romashka⟧-billing/⟦keep:2.14.3⟧")
}
