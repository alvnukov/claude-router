package platform

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// x/sys has no wrappers for the credential API, so the four calls are bound
// here. No cgo and no cmdkey: the secret never leaves this process.
var (
	advapi32       = windows.NewLazySystemDLL("advapi32.dll")
	procCredWrite  = advapi32.NewProc("CredWriteW")
	procCredRead   = advapi32.NewProc("CredReadW")
	procCredDelete = advapi32.NewProc("CredDeleteW")
	procCredFree   = advapi32.NewProc("CredFree")
)

const (
	credTypeGeneric         = 1
	credPersistLocalMachine = 2
)

// credential is CREDENTIALW, field for field.
type credential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

// OSSecretStore keeps secrets as generic credentials of the current user in
// Credential Manager, one per "service/account" target.
func OSSecretStore(service string) (SecretStore, error) {
	if err := checkSecretName("service", service); err != nil {
		return nil, err
	}
	return &credentials{service: service}, nil
}

type credentials struct{ service string }

func (c *credentials) Put(account string, secret []byte) error {
	target, err := c.target(account)
	if err != nil {
		return err
	}
	if len(secret) == 0 {
		return errors.New("platform: empty secret")
	}
	blob := append([]byte(nil), secret...)
	cred := credential{
		Type:               credTypeGeneric,
		TargetName:         target,
		CredentialBlobSize: uint32(len(blob)),
		CredentialBlob:     &blob[0],
		Persist:            credPersistLocalMachine,
	}
	r, _, callErr := procCredWrite.Call(uintptr(unsafe.Pointer(&cred)), 0)
	runtime.KeepAlive(blob)
	runtime.KeepAlive(target)
	clear(blob)
	if r == 0 {
		return fmt.Errorf("platform: credential write %s: %w", account, callErr)
	}
	return nil
}

func (c *credentials) Get(account string) ([]byte, error) {
	target, err := c.target(account)
	if err != nil {
		return nil, err
	}
	var cred *credential
	r, _, callErr := procCredRead.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&cred)))
	runtime.KeepAlive(target)
	if r == 0 {
		if errors.Is(callErr, windows.ERROR_NOT_FOUND) {
			return nil, ErrSecretNotFound
		}
		return nil, fmt.Errorf("platform: credential read %s: %w", account, callErr)
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(cred)))
	if cred.CredentialBlobSize == 0 || cred.CredentialBlob == nil {
		return nil, fmt.Errorf("platform: credential read %s: empty secret", account)
	}
	return append([]byte(nil), unsafe.Slice(cred.CredentialBlob, cred.CredentialBlobSize)...), nil
}

func (c *credentials) Delete(account string) error {
	target, err := c.target(account)
	if err != nil {
		return err
	}
	r, _, callErr := procCredDelete.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0)
	runtime.KeepAlive(target)
	if r == 0 && !errors.Is(callErr, windows.ERROR_NOT_FOUND) {
		return fmt.Errorf("platform: credential delete %s: %w", account, callErr)
	}
	return nil
}

func (c *credentials) target(account string) (*uint16, error) {
	if err := checkSecretName("account", account); err != nil {
		return nil, err
	}
	return windows.UTF16PtrFromString(c.service + "/" + account)
}
