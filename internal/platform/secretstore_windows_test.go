package platform

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
)

// secretTestEnv turns on the tests that write to the real OS secret store. CI
// sets it; under it an unavailable store fails the test instead of skipping.
const secretTestEnv = "ROUTER_OS_SECRET_TEST"

func TestCredentialSecretStore(t *testing.T) {
	if os.Getenv(secretTestEnv) != "1" {
		t.Skip(secretTestEnv + "=1 runs the Credential Manager tests")
	}
	// A fresh service per run, so a leftover from a crashed run never answers.
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	store, err := OSSecretStore("claude-router-privacy-test-" + hex.EncodeToString(suffix))
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"roundtrip", "deleted", "overwrite"} {
		t.Cleanup(func() { _ = store.Delete(account) })
	}
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
}
