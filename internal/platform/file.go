package platform

import "os"

// MkdirPrivate creates dir and its missing parents, readable only by the
// user on unix. On Windows mode bits do not apply: the directory inherits the
// ACL of its parent, which for the router's files is the user's profile.
func MkdirPrivate(dir string) error {
	return os.MkdirAll(dir, 0o700)
}
