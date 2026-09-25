package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"localrouter/internal/limits"
)

// Anthropic subscription limits are observed passively from the
// anthropic-ratelimit-* headers on the answers the proxy relays; internal/limits
// keeps them and limits.json. GET /api/limits puts them next to Codex usage.

// limitsReport is GET /api/limits: the state of every source and one list of
// windows, each with its source and when it was observed. docs/features.md
// describes this JSON.
type limitsReport struct {
	Sources []limitSource   `json:"sources"`
	Windows []limits.Window `json:"windows"`
}

type limitSource struct {
	Source        string            `json:"source"`             // anthropic | codex
	Provider      string            `json:"provider,omitempty"` // Codex connection name
	State         string            `json:"state"`              // fresh | no_headers | unavailable | not_connected
	ObservedAt    time.Time         `json:"observed_at,omitzero"`
	AgeSeconds    *int64            `json:"age_seconds,omitempty"`
	MaxAgeSeconds int64             `json:"max_age_seconds"`
	Raw           map[string]string `json:"raw,omitzero"`
	Error         string            `json:"error,omitempty"`
}

// limitsReportOf puts Anthropic and every Codex connection in one report, a
// codex source per connection named by provider; with no Codex connection
// there is one codex source, not_connected. Codex windows come from the last
// successful usage request and are shown while it is at most
// codexUsageMaxAge old; a later failure is reported next to them.
func limitsReportOf(a limits.View, codex []codexUsageView, now time.Time) limitsReport {
	r := limitsReport{Windows: []limits.Window{}}
	r.Sources = append(r.Sources, limitSource{Source: "anthropic", State: a.State, ObservedAt: a.ObservedAt,
		AgeSeconds: a.AgeSeconds, MaxAgeSeconds: a.MaxAgeSeconds, Raw: a.Raw})
	for _, w := range a.Windows {
		w.Source, w.ObservedAt = "anthropic", a.ObservedAt
		r.Windows = append(r.Windows, w)
	}

	if len(codex) == 0 {
		codex = []codexUsageView{{}}
	}
	for _, c := range codex {
		r.codexSource(c, now)
	}
	return r
}

func (r *limitsReport) codexSource(c codexUsageView, now time.Time) {
	s := limitSource{Source: "codex", Provider: c.Provider, State: "not_connected", MaxAgeSeconds: int64(codexUsageMaxAge / time.Second), Error: c.Error}
	if c.Connected {
		s.State = "unavailable"
	}
	if c.Connected && !c.Updated.IsZero() {
		age := now.Sub(c.Updated)
		s.ObservedAt = c.Updated
		seconds := max(int64(age/time.Second), 0)
		s.AgeSeconds = &seconds
		if age >= -limits.ClockSkew && age <= codexUsageMaxAge {
			s.State = "fresh"
			for _, row := range c.Limits {
				w := limits.Window{Source: "codex", Provider: c.Provider, Name: row.ID, WindowSeconds: row.Seconds, ObservedAt: c.Updated}
				if row.Known {
					remaining, used := row.Remaining, row.Used
					w.RemainingPercent, w.UsedPercent = &remaining, &used
				}
				if !row.Reset.IsZero() {
					w.ResetAt = row.Reset.UTC()
				}
				if row.Blocked {
					w.Status = "rejected"
				}
				r.Windows = append(r.Windows, w)
			}
		}
	}
	r.Sources = append(r.Sources, s)
}

func limitsPath() string {
	if v, ok := os.LookupEnv("ROUTER_ANTHROPIC_LIMITS_FILE"); ok {
		return v // "" disables
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "limits.json")
	}
	return "limits.json"
}

// limitsAPI is GET /api/limits: what the settings page shows, from the
// stores only; nothing here reaches Anthropic or Codex.
func (u *uiServer) limitsAPI(w http.ResponseWriter, r *http.Request) {
	var codex []codexUsageView
	for _, t := range u.codexUsageTargets() {
		v := t.cache.get(r.Context(), t.auth, false)
		v.Provider = t.provider
		codex = append(codex, v)
	}
	now := time.Now()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(limitsReportOf(u.limits.View(now), codex, now)) // the client may be gone
}
