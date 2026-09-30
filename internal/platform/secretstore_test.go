package platform

import "testing"

func TestCheckSecretName(t *testing.T) {
	for _, name := range []string{"claude-router-privacy", "a.b_c/d-0"} {
		if err := checkSecretName("account", name); err != nil {
			t.Errorf("checkSecretName(%q) = %v; want nil", name, err)
		}
	}
	// Anything a store's command line would have to quote is refused.
	for _, name := range []string{"", "a b", `a"b`, `a\b`, "a\nb", "a'b", "имя"} {
		if err := checkSecretName("account", name); err == nil {
			t.Errorf("checkSecretName(%q) = nil; want an error", name)
		}
	}
}
