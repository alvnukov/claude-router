package platform

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// allBytes locks the whole file, whatever its length.
const allBytes = ^uint32(0)

// tryLock takes an exclusive LockFileEx lock on f without waiting; false means
// another holder has it.
func tryLock(f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, allBytes, allBytes, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

// unlock releases the lock. Closing f would release it too, so a failure here
// leaves nothing held.
func unlock(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, allBytes, allBytes, new(windows.Overlapped))
}
