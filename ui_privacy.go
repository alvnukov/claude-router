package main

import (
	"localrouter/internal/privacy"
	"path/filepath"
)

// PrivacyLab has an independent lifetime and never writes request history or
// touches routing. Its configuration lives beside this router's providers.json.
func (u *uiServer) PrivacyLab() *privacy.Lab {
	u.privacyOnce.Do(func() {
		home := ""
		if u.cs.provPath != "" {
			home = filepath.Dir(u.cs.provPath)
		}
		u.privacyLab = privacy.NewLab(home)
	})
	return u.privacyLab
}
func (b uiBackend) PrivacyLab() *privacy.Lab { return b.u.PrivacyLab() }

func (b uiBackend) PrivacyRuntime() *privacy.Runtime { return b.u.cs.privacyRuntime() }
