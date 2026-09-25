package platform

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// replaceWait bounds how long ReplaceFile retries a rename that the OS refuses
// while another handle has the target open.
const replaceWait = time.Second

// MkdirPrivate creates dir and its missing parents, readable only by the
// user on unix. On Windows mode bits do not apply: the directory inherits the
// ACL of its parent, which for the router's files is the user's profile.
func MkdirPrivate(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

// WriteFileAtomic replaces path with data so that a reader sees the old file
// or the new one, never a partial write. The data goes to a temporary file
// ".<base>.*" in path's directory, which must exist; it is synced to disk and
// renamed over path with ReplaceFile. The directory is not synced, so a crash
// right after the rename may leave the old file. On failure path is left as it
// was and the temporary file is removed.
//
// On unix the file gets mode perm whatever the umask. On Windows access comes
// from the directory's ACL, and of perm only the owner-write bit applies:
// without it the file is read-only.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = f.Chmod(perm)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return ReplaceFile(f.Name(), path)
}

// ReplaceFile renames src over dst. Unix replaces dst even while it is open.
// Windows refuses while another handle has dst open, so the rename is retried
// for up to a second before its error is returned.
func ReplaceFile(src, dst string) error {
	return renameRetrying(os.Rename, renameBusy, replaceWait, src, dst)
}

// renameRetrying calls rename until it succeeds, fails with an error busy does
// not accept, or wait has passed; it returns rename's last error.
func renameRetrying(rename func(src, dst string) error, busy func(err error, dst string) bool, wait time.Duration, src, dst string) error {
	deadline := time.Now().Add(wait)
	for {
		err := rename(src, dst)
		if err == nil || !busy(err, dst) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(pollInterval)
	}
}
