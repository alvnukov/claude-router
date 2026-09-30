package platform

import (
	"errors"
	"strings"
	"syscall"
	"testing"
)

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

func TestCredentialError(t *testing.T) {
	if err := credentialError("read", "acct", errCredNotFound); err != ErrSecretNotFound {
		t.Errorf("not found = %v; want ErrSecretNotFound", err)
	}
	// An ssh logon by key has no credential vault: the store is absent, and
	// the reason says why.
	err := credentialError("write", "acct", errCredNoLogonSession)
	if !errors.Is(err, ErrNoLogonSession) || !errors.Is(err, ErrNoSecretStore) {
		t.Errorf("no logon session = %v; want ErrNoLogonSession and ErrNoSecretStore", err)
	}
	if !strings.Contains(err.Error(), "logon session") {
		t.Errorf("no logon session = %q; want the reason in the text", err)
	}
	other := syscall.Errno(5)
	if err := credentialError("write", "acct", other); !errors.Is(err, other) || errors.Is(err, ErrNoSecretStore) || errors.Is(err, ErrSecretNotFound) {
		t.Errorf("other = %v; want it wrapped as is", err)
	}
}
