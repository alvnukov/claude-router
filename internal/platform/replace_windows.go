package platform

import (
	"errors"

	"golang.org/x/sys/windows"
)

// renameBusy reports a rename refused because another handle has one of the
// files open: MoveFileEx fails with access denied while the target is open,
// and with a sharing violation while the source is, as a virus scanner may
// hold a freshly written file.
func renameBusy(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
