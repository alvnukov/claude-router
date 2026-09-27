package privacy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"localrouter/internal/platform"
)

type ProfileSnapshot struct {
	Status   string    `json:"status"`
	Revision string    `json:"revision"`
	Config   *Profiles `json:"config"`
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("invalid_file")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("invalid_file")
	}
	return b, nil
}
func (l *Lab) Config() ProfileSnapshot {
	s := ProfileSnapshot{Status: "missing", Revision: "missing", Config: &Profiles{Version: 1, Profiles: []FilterProfile{}, Bindings: []Binding{}}}
	if l.home == "" {
		s.Status = "unavailable"
		return s
	}
	body, err := readBounded(filepath.Join(l.home, "privacy-profiles.json"), 256<<10)
	if errors.Is(err, os.ErrNotExist) {
		return s
	}
	s.Status, s.Config, s.Revision = "invalid", nil, ""
	if err != nil {
		return s
	}
	sum := sha256.Sum256(body)
	s.Revision = hex.EncodeToString(sum[:])
	s.Config, err = ParseProfiles(body)
	if err == nil {
		s.Status = "ready"
	}
	return s
}

// SaveConfig protects against concurrent UI/process edits and replaces only a
// fully validated snapshot. No mutation affects a preview already in progress.
func (l *Lab) SaveConfig(ctx context.Context, revision string, body []byte) (ProfileSnapshot, error) {
	var saved ProfileSnapshot
	if l.home == "" {
		return saved, errors.New("unavailable")
	}
	c, err := ParseProfiles(body)
	if err != nil {
		return saved, errors.New("invalid_profiles")
	}
	canonical, err := json.MarshalIndent(c, "", "  ")
	if err != nil || len(canonical) > 256<<10 {
		return saved, errors.New("invalid_profiles")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err = platform.WithLock(ctx, filepath.Join(l.home, ".privacy-profiles.lock"), func() error {
		current := l.Config()
		if revision == "" || current.Revision != revision {
			return errors.New("conflict")
		}
		if err := platform.WritePrivateAtomic(filepath.Join(l.home, "privacy-profiles.json"), canonical); err != nil {
			return err
		}
		sum := sha256.Sum256(canonical)
		saved = ProfileSnapshot{Status: "ready", Revision: hex.EncodeToString(sum[:]), Config: c}
		return nil
	})
	if err != nil && err.Error() != "conflict" {
		return saved, errors.New("save_failed")
	}
	return saved, err
}

// LegacyRules is an explicit local import, not part of aggregate observability.
// No caller can supply a path. Errors never echo rule contents or filenames.
func (l *Lab) LegacyRules() (json.RawMessage, error) {
	if l.home == "" {
		return nil, errors.New("unavailable")
	}
	body, err := readBounded(filepath.Join(l.home, "privacy.json"), 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("rules_missing")
	}
	if err != nil {
		return nil, errors.New("invalid_rules")
	}
	if _, err := ParseRules(body); err != nil {
		return nil, errors.New("invalid_rules")
	}
	return json.RawMessage(body), nil
}
