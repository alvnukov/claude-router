package platform

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// renameBusy reports a rename refused because another handle has one of the
// files open: MoveFileEx fails with a sharing violation while the source is
// open, as a virus scanner may hold a freshly written file, and with access
// denied while the target is. Access denied on a directory or a read-only
// target is final.
func renameBusy(err error, dst string) bool {
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return true
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return false
	}
	info, statErr := os.Lstat(dst)
	return statErr == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o200 != 0
}
