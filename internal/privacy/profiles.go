package privacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// Profiles is a versioned configuration snapshot, independent of connection
// profiles. A pool target includes its connection profile: "work/fast".
// Resolution chooses ONE policy, never a union of rules or dictionaries.
type Profiles struct {
	Version  int             `json:"version"`
	Enabled  bool            `json:"enabled"`
	Default  string          `json:"default"`
	Profiles []FilterProfile `json:"profiles"`
	Bindings []Binding       `json:"bindings"`
}
type FilterProfile struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Enabled bool            `json:"enabled"`
	Mode    FilterMode      `json:"mode,omitempty"`
	Rules   json.RawMessage `json:"rules"`
}

type FilterMode string

const (
	ModeMask   FilterMode = "mask"
	ModeDetect FilterMode = "detect"
)

func effectiveMode(mode FilterMode) FilterMode {
	if mode == "" {
		return ModeMask
	}
	return mode
}

type Binding struct {
	Kind    string `json:"kind"`
	Target  string `json:"target"`
	Profile string `json:"profile"`
}
type Target struct {
	Model    string `json:"model"`
	Pool     string `json:"pool"`
	Provider string `json:"provider"`
}
type Resolution struct {
	Profile string     `json:"profile"`
	Via     string     `json:"via"`
	Enabled bool       `json:"enabled"`
	Mode    FilterMode `json:"mode"`
}

var profileID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// ParseProfiles validates all profiles, including disabled ones. Errors never
// echo configuration values, which can contain credentials and identities.
func ParseProfiles(body []byte) (*Profiles, error) {
	invalid := errors.New("invalid_profiles")
	if len(body) > 256<<10 {
		return nil, invalid
	}
	if _, err := scanJSON(body); err != nil {
		return nil, invalid
	}
	var c Profiles
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || c.Version != 1 || len(c.Profiles) > 32 || len(c.Bindings) > 256 {
		return nil, invalid
	}
	ids := map[string]bool{}
	for _, p := range c.Profiles {
		if mode := effectiveMode(p.Mode); mode != ModeMask && mode != ModeDetect {
			return nil, invalid
		}
		if !profileID.MatchString(p.ID) || ids[p.ID] || strings.TrimSpace(p.Name) == "" || len(p.Name) > 160 || len(p.Rules) > 64<<10 {
			return nil, invalid
		}
		if _, err := ParseRules(p.Rules); err != nil {
			return nil, invalid
		}
		ids[p.ID] = true
	}
	if c.Default != "" && !ids[c.Default] {
		return nil, invalid
	}
	bindings := map[string]bool{}
	for _, b := range c.Bindings {
		key := b.Kind + "\x00" + b.Target
		if (b.Kind != "model" && b.Kind != "pool" && b.Kind != "provider") || !ids[b.Profile] || bindings[key] || b.Target == "" || len(b.Target) > 512 || strings.TrimSpace(b.Target) != b.Target || strings.ContainsAny(b.Target, "\x00\r\n") {
			return nil, invalid
		}
		bindings[key] = true
	}
	if c.Profiles == nil {
		c.Profiles = []FilterProfile{}
	}
	if c.Bindings == nil {
		c.Bindings = []Binding{}
	}
	return &c, nil
}

// Resolve requires validated configuration and the actual selected destination,
// after routing. Failover must resolve again, before sending any request bytes.
// Enabled=false is an explicit bypass, including a disabled matching profile.
func (c *Profiles) Resolve(t Target) Resolution {
	r := Resolution{Profile: c.Default, Via: "default", Mode: ModeMask}
	for _, match := range []struct{ kind, value string }{{"model", t.Model}, {"pool", t.Pool}, {"provider", t.Provider}} {
		found := false
		for _, b := range c.Bindings {
			if b.Kind == match.kind && b.Target == match.value {
				r.Profile, r.Via, found = b.Profile, b.Kind, true
				break
			}
		}
		if found {
			break
		}
	}
	for _, p := range c.Profiles {
		if p.ID == r.Profile {
			r.Enabled = c.Enabled && p.Enabled
			r.Mode = effectiveMode(p.Mode)
			return r
		}
	}
	r.Via = "none"
	return r
}
