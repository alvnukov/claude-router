package uisession

import "localrouter/internal/privacy"

// PromptTitles reports whether a session may be titled from request text. With
// privacy profiles on, or a policy that fails to load, request text stays out
// of the session list and the UI falls back to the id and the journal context.
func PromptTitles(rt *privacy.Runtime) bool {
	if rt == nil {
		return true
	}
	_, _ = rt.Snapshot()
	state := rt.State()
	return !state.Enabled && state.Status != "invalid"
}
