//go:build !unix

package platform

import "errors"

// StartDetached has no implementation here before the Windows service lands.
func StartDetached(exe string, args []string, logPath string) (int, error) {
	return 0, errors.ErrUnsupported
}

// Terminate has no implementation here before the Windows service lands.
func Terminate(pid int) error { return errors.ErrUnsupported }

// KillMatching has no implementation here before the Windows service lands.
func KillMatching(pattern string) error { return errors.ErrUnsupported }
