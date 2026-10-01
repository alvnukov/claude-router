package config

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

const CodexBaseURL = "https://chatgpt.com/backend-api/codex"

// NewCodexAuthID names one connection's credential slot for its whole life.
func NewCodexAuthID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func AuthIDOK(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// assignCodexAuthIDs is the one-time startup migration: only a pre-existing
// non-legacy Codex provider without an id gets one. The legacy "codex" keeps
// the old credential file and needs no id.
func assignCodexAuthIDs(l *Local) bool {
	changed := false
	for i := range l.Providers {
		p := &l.Providers[i]
		if p.Type == "codex" && p.AuthID == "" && strings.TrimSpace(p.Name) != "codex" {
			p.AuthID = NewCodexAuthID()
			changed = true
		}
	}
	return changed
}
