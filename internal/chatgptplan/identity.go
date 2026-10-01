package chatgptplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"
)

const (
	AuthMode     = "chatgpt-plan"
	Issuer       = "https://auth.openai.com"
	Resource     = "https://api.openai.com/v1"
	ModelsURL    = Resource + "/models"
	ResponsesURL = Resource + "/responses"
)

type Registration struct {
	Issuer            string    `json:"issuer"`
	Subject           string    `json:"subject"`
	ClientID          string    `json:"client_id"`
	HostID            string    `json:"host_id"`
	Email             string    `json:"email,omitempty"`
	Name              string    `json:"name,omitempty"`
	Scopes            []string  `json:"scopes"`
	ExpiresAt         time.Time `json:"expires_at"`
	EarliestRefreshAt time.Time `json:"earliest_refresh_at,omitempty"`
}

type Tokens struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
}

func (r Registration) SharingEnabled() bool {
	return slices.Contains(r.Scopes, "chatgpt.tokens.use.direct")
}
func (r Registration) IdentityKey() string {
	data, _ := json.Marshal([]string{r.Issuer, r.Subject, r.ClientID})
	digest := sha256.Sum256(data)
	return "chatgpt-plan:" + hex.EncodeToString(digest[:])
}
func (r Registration) Valid() bool {
	return validIssuer(r.Issuer) && r.Subject != "" && len(r.Subject) <= 1024 && validClientID(r.ClientID) && validHostID(r.HostID) && !r.ExpiresAt.IsZero()
}
