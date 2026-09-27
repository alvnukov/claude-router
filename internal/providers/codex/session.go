package codex

import (
	"crypto/sha256"
	"encoding/hex"
)

// SessionKey provides stable ChatGPT cache affinity without putting arbitrary
// client metadata in HTTP headers or assigning unrelated requests one session.
func SessionKey(session string) string {
	if session == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("claude-router:" + session))
	return hex.EncodeToString(sum[:])
}
