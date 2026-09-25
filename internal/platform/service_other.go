//go:build !darwin

package platform

import "errors"

// NewService has no implementation on this OS yet.
func NewService() (Service, error) {
	return nil, errors.ErrUnsupported
}
