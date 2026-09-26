//go:build !darwin

package platform

import (
	"errors"
	"testing"
)

func TestNewServiceIsUnsupported(t *testing.T) {
	if _, err := NewService(); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("NewService() error = %v; want ErrUnsupported", err)
	}
}
