package main

import (
	"localrouter/internal/privacy"
)

// PrivacyLab has an independent lifetime and never writes request history or
// touches routing. Its configuration lives beside this router's providers.json.
func (u *uiServer) PrivacyLab() *privacy.Lab {
	u.privacyOnce.Do(func() {
		u.privacyLab = privacy.NewLab(u.cs.Home())
	})
	return u.privacyLab
}
func (b uiBackend) PrivacyLab() *privacy.Lab { return b.u.PrivacyLab() }

func (b uiBackend) PrivacyRuntime() *privacy.Runtime { return b.u.cs.PrivacyRuntime() }
