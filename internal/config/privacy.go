package config

import (
	"path/filepath"

	"localrouter/internal/privacy"
)

// Path is the providers.json this store reads and writes, or "" for none.
func (s *Store) Path() string { return s.provPath }

// Home is the directory of providers.json, or "" for a store without that
// file. Privacy settings live beside it.
func (s *Store) Home() string {
	if s.provPath == "" {
		return ""
	}
	return filepath.Dir(s.provPath)
}

// PrivacyRuntime is the privacy runtime of this configuration home.
func (s *Store) PrivacyRuntime() *privacy.Runtime {
	s.privacyOnce.Do(func() { s.privacy = privacy.NewRuntime(s.Home()) })
	return s.privacy
}
