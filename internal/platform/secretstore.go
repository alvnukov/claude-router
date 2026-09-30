package platform

import (
	"errors"
	"fmt"
)

var (
	// ErrNoSecretStore means this OS has no secret store the router can use.
	ErrNoSecretStore = errors.New("platform: no OS secret store")
	// ErrSecretNotFound means the store holds no secret for the account.
	ErrSecretNotFound = errors.New("platform: secret not found")
	// ErrNoLogonSession means the OS has a store, but this process runs
	// without a user logon session that could open it (a Windows service
	// without a profile, an ssh logon by key). It is an ErrNoSecretStore.
	ErrNoLogonSession = fmt.Errorf("%w: no user logon session to open it", ErrNoSecretStore)
)

// SecretStore keeps small secrets in the OS secret store, one per account
// under a fixed service. A secret never passes through a command line.
type SecretStore interface {
	Put(account string, secret []byte) error
	// Get returns ErrSecretNotFound when the account has no secret.
	Get(account string) ([]byte, error)
	// Delete of an account with no secret is not an error.
	Delete(account string) error
}

// checkSecretName admits service and account names made of letters, digits
// and "-._/", so they need no quoting on any store's command line.
func checkSecretName(what, name string) error {
	if name == "" {
		return fmt.Errorf("platform: empty secret %s", what)
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.', c == '_', c == '/':
		default:
			return fmt.Errorf("platform: secret %s %q has a character outside [A-Za-z0-9._/-]", what, name)
		}
	}
	return nil
}
