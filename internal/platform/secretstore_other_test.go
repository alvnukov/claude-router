//go:build !darwin && !windows

package platform

import (
	"errors"
	"testing"
)

func TestNoSecretStoreElsewhere(t *testing.T) {
	if store, err := OSSecretStore("claude-router-privacy-test"); store != nil || !errors.Is(err, ErrNoSecretStore) {
		t.Fatalf("OSSecretStore = %v, %v; want nil, ErrNoSecretStore", store, err)
	}
}
