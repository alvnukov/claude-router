package platform

import (
	"bytes"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// securityNotFound is the exit code of security(1) for a missing item.
const securityNotFound = 44

// OSSecretStore keeps secrets as generic passwords in the login keychain.
func OSSecretStore(service string) (SecretStore, error) {
	if err := checkSecretName("service", service); err != nil {
		return nil, err
	}
	return &keychain{service: service, tool: "/usr/bin/security"}, nil
}

// keychain drives security(1). file names the keychain; "" is the login
// keychain. Writes go through "security -i" on stdin, so the secret never
// appears in an argv that other processes can list.
type keychain struct{ service, tool, file string }

func (k *keychain) Put(account string, secret []byte) error {
	if err := k.check(account); err != nil {
		return err
	}
	if len(secret) == 0 {
		return errors.New("platform: empty secret")
	}
	encoded := hex.EncodeToString(secret)
	line := "add-generic-password -U -s " + k.service + " -a " + account + " -w " + encoded
	if k.file != "" {
		line += ` "` + k.file + `"`
	}
	cmd := exec.Command(k.tool, "-i")
	cmd.Stdin = strings.NewReader(line + "\n")
	// Output is dropped unread: an error message may repeat the command line.
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("platform: keychain write %s: %w", account, exitOnly(err))
	}
	// Interactive mode can report success for a command that did not take, so
	// the write counts only once it reads back.
	got, err := k.Get(account)
	if err != nil {
		return fmt.Errorf("platform: keychain write %s: read back: %w", account, err)
	}
	if subtle.ConstantTimeCompare(got, secret) != 1 {
		return fmt.Errorf("platform: keychain write %s: read back a different secret", account)
	}
	return nil
}

func (k *keychain) Get(account string) ([]byte, error) {
	if err := k.check(account); err != nil {
		return nil, err
	}
	out, err := exec.Command(k.tool, k.args("find-generic-password", account, "-w")...).Output()
	if code := securityExit(err); code == securityNotFound {
		return nil, ErrSecretNotFound
	} else if err != nil {
		return nil, fmt.Errorf("platform: keychain read %s: %w", account, exitOnly(err))
	}
	secret, err := hex.DecodeString(string(bytes.TrimSpace(out)))
	if err != nil || len(secret) == 0 {
		return nil, fmt.Errorf("platform: keychain read %s: not a secret written by the router", account)
	}
	return secret, nil
}

func (k *keychain) Delete(account string) error {
	if err := k.check(account); err != nil {
		return err
	}
	err := exec.Command(k.tool, k.args("delete-generic-password", account)...).Run()
	if code := securityExit(err); code == securityNotFound {
		return nil
	} else if err != nil {
		return fmt.Errorf("platform: keychain delete %s: %w", account, exitOnly(err))
	}
	return nil
}

// check refuses an account name that needs quoting and a named keychain file
// that is absent: security(1) would silently fall back to the login keychain.
func (k *keychain) check(account string) error {
	if err := checkSecretName("account", account); err != nil {
		return err
	}
	if k.file == "" {
		return nil
	}
	if strings.ContainsAny(k.file, "\"\\\n") {
		return fmt.Errorf("platform: keychain path %q needs quoting", k.file)
	}
	if _, err := os.Stat(k.file); err != nil {
		return fmt.Errorf("platform: keychain %w", err)
	}
	return nil
}

func (k *keychain) args(command, account string, extra ...string) []string {
	args := append([]string{command, "-s", k.service, "-a", account}, extra...)
	if k.file != "" {
		args = append(args, k.file)
	}
	return args
}

func securityExit(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return 0
}

// exitOnly keeps an exec error to its exit status: stderr of security(1) is
// never quoted into errors that reach logs.
func exitOnly(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Errorf("security exited %d", exit.ExitCode())
	}
	return err
}
