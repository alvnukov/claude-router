package platform

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// secretTestEnv turns on the tests that write to the real OS secret store. CI
// sets it; under it an unavailable store fails the test instead of skipping.
const secretTestEnv = "ROUTER_OS_SECRET_TEST"

func TestKeychainSecretStore(t *testing.T) {
	if os.Getenv(secretTestEnv) != "1" {
		t.Skip(secretTestEnv + "=1 runs the Keychain tests")
	}
	// A throwaway keychain, so the login keychain is never touched.
	file := filepath.Join(t.TempDir(), "t.keychain")
	security := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("/usr/bin/security", args...).CombinedOutput(); err != nil {
			t.Fatalf("security %s: %v: %s", args[0], err, out)
		}
	}
	security("create-keychain", "-p", "test", file)
	t.Cleanup(func() { _ = exec.Command("/usr/bin/security", "delete-keychain", file).Run() })
	security("unlock-keychain", "-p", "test", file)
	store := &keychain{service: "claude-router-privacy-test", tool: "/usr/bin/security", file: file}
	key := bytes.Repeat([]byte{0xa5, 0x01}, 16)

	t.Run("put get roundtrip", func(t *testing.T) {
		if err := store.Put("roundtrip", key); err != nil {
			t.Fatal(err)
		}
		got, err := store.Get("roundtrip")
		if err != nil || !bytes.Equal(got, key) {
			t.Fatalf("Get = %x, %v; want %x", got, err, key)
		}
	})
	t.Run("missing is ErrSecretNotFound", func(t *testing.T) {
		if got, err := store.Get("missing"); !errors.Is(err, ErrSecretNotFound) {
			t.Fatalf("Get(missing) = %x, %v; want ErrSecretNotFound", got, err)
		}
	})
	t.Run("delete missing is nil", func(t *testing.T) {
		if err := store.Delete("missing"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("delete removes", func(t *testing.T) {
		if err := store.Put("deleted", key); err != nil {
			t.Fatal(err)
		}
		if err := store.Delete("deleted"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Get("deleted"); !errors.Is(err, ErrSecretNotFound) {
			t.Fatalf("Get after Delete: %v; want ErrSecretNotFound", err)
		}
	})
	t.Run("put overwrites", func(t *testing.T) {
		other := bytes.Repeat([]byte{0x3c}, 32)
		if err := store.Put("overwrite", key); err != nil {
			t.Fatal(err)
		}
		if err := store.Put("overwrite", other); err != nil {
			t.Fatal(err)
		}
		if got, err := store.Get("overwrite"); err != nil || !bytes.Equal(got, other) {
			t.Fatalf("Get = %x, %v; want %x", got, err, other)
		}
	})
	t.Run("missing keychain file refused", func(t *testing.T) {
		// security falls back to the login keychain when the named file is
		// absent; the store must refuse instead of writing there.
		gone := &keychain{service: store.service, tool: store.tool, file: filepath.Join(t.TempDir(), "absent.keychain")}
		// Should the store regress, the stray item is taken out again.
		t.Cleanup(func() {
			_ = exec.Command("/usr/bin/security", "delete-generic-password", "-s", store.service, "-a", "fallback").Run()
		})
		if err := gone.Put("fallback", key); err == nil {
			t.Fatal("Put into an absent keychain file succeeded")
		}
		// An error alone is not enough: the read back fails too, after the
		// write has already landed in the default keychain.
		err := exec.Command("/usr/bin/security", "find-generic-password", "-s", store.service, "-a", "fallback").Run()
		if securityExit(err) != securityNotFound {
			t.Fatalf("an item reached the default keychain (find: %v)", err)
		}
	})
}

func TestKeychainSecretNotInArgv(t *testing.T) {
	// A stand-in for security that logs its argv and stdin and answers a read
	// with the last secret written, so Put can check what it wrote.
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	tool := filepath.Join(dir, "security")
	script := `#!/bin/sh
printf 'argv: %s\n' "$*" >> '` + log + `'
if [ "$1" = "-i" ]; then sed 's/^/stdin: /' >> '` + log + `'; exit 0; fi
if [ "$1" = "find-generic-password" ]; then sed -n 's/^stdin: .* -w \([0-9a-f]*\).*/\1/p' '` + log + `' | tail -n 1; fi
exit 0
`
	if err := os.WriteFile(tool, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	store := &keychain{service: "claude-router-privacy-test", tool: tool}
	key := bytes.Repeat([]byte{0x5e, 0xc7}, 16)
	if err := store.Put("argv", key); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	secret := hex.EncodeToString(key)
	var inArgv, inStdin bool
	for _, line := range strings.Split(string(data), "\n") {
		inArgv = inArgv || strings.HasPrefix(line, "argv: ") && strings.Contains(line, secret)
		inStdin = inStdin || strings.HasPrefix(line, "stdin: ") && strings.Contains(line, secret)
	}
	if inArgv || !inStdin {
		t.Fatalf("secret in argv %v, in stdin %v; want only in stdin:\n%s", inArgv, inStdin, data)
	}
}

func TestKeychainWriteVerified(t *testing.T) {
	// security -i exits 0 even when its command fails; a write that does not
	// read back is an error.
	tool := filepath.Join(t.TempDir(), "security")
	script := "#!/bin/sh\nif [ \"$1\" = \"-i\" ]; then cat >/dev/null; exit 0; fi\nexit 44\n"
	if err := os.WriteFile(tool, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	store := &keychain{service: "claude-router-privacy-test", tool: tool}
	if err := store.Put("lost", bytes.Repeat([]byte{1}, 32)); err == nil {
		t.Fatal("Put succeeded although the secret does not read back")
	}
}
