package platform

import (
	"errors"
	"fmt"
	"syscall"
)

// Credential Manager errors as bare errnos, so the mapping below compiles and
// is tested on every OS and not only on Windows.
const (
	errCredNotFound       = syscall.Errno(1168) // ERROR_NOT_FOUND
	errCredNoLogonSession = syscall.Errno(1312) // ERROR_NO_SUCH_LOGON_SESSION
)

// credentialError turns a failed credential call into the store's errors.
func credentialError(op, account string, err error) error {
	switch {
	case errors.Is(err, errCredNotFound):
		return ErrSecretNotFound
	case errors.Is(err, errCredNoLogonSession):
		return fmt.Errorf("platform: credential %s %s: %w", op, account, ErrNoLogonSession)
	}
	return fmt.Errorf("platform: credential %s %s: %w", op, account, err)
}
