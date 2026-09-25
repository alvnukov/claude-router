package platform

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// lockPoll is how often WithLock tries a lock that another holder has.
const lockPoll = 10 * time.Millisecond

// WithLock runs fn holding an exclusive lock on the file at path. The lock is
// shared by every process that locks the same path and is released when fn
// returns. The file and its directory are created if missing.
//
// A lock that another holder has is tried again every 10 ms until ctx is
// done, and then WithLock returns ctx.Err(). The first try is made even if
// ctx is done already, so a done ctx asks for exactly one try.
func WithLock(ctx context.Context, path string, fn func() error) error {
	if err := MkdirPrivate(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		locked, err := tryLock(f)
		if err != nil {
			return err
		}
		if locked {
			defer unlock(f)
			return fn()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}
