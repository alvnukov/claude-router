package privacy

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// cleanLegacy removes session files of the earlier formats under runtimeDir:
// they hold real values. A file is judged by its path and name alone and is
// never opened. Only regular files on a legacy path go; a symlink, an unknown
// file or a file on an unexpected path stays and is counted, so a leftover is
// visible instead of silently lost. Directories left empty afterwards go too.
func cleanLegacy(runtimeDir string) (removed, unknown int, err error) {
	var dirs []string
	err = filepath.WalkDir(runtimeDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == runtimeDir && errors.Is(walkErr, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return walkErr
		}
		rel, err := filepath.Rel(runtimeDir, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if d.IsDir() {
			if path != runtimeDir && parts[0] != "sessions" {
				dirs = append(dirs, path)
			}
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case legacySessionPath(parts) && info.Mode().IsRegular():
			if err := os.Remove(path); err != nil {
				return err
			}
			removed++
		case currentSessionPath(parts):
		default:
			unknown++
		}
		return nil
	})
	if err != nil {
		return removed, unknown, err
	}
	// Deepest first, so a directory is empty by the time its parent is tried;
	// a directory still holding an unknown file stays.
	slices.Reverse(dirs)
	for _, dir := range dirs {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
			if err := os.Remove(dir); err != nil {
				return removed, unknown, err
			}
		}
	}
	return removed, unknown, nil
}

// legacySessionPath matches <namespace>/privacy/sessions/<file> and
// <namespace>/privacy/sessions/v2/<file>.
func legacySessionPath(parts []string) bool {
	switch {
	case len(parts) == 4 && parts[1] == "privacy" && parts[2] == "sessions":
		return legacySessionFile(parts[3])
	case len(parts) == 5 && parts[1] == "privacy" && parts[2] == "sessions" && parts[3] == "v2":
		return legacySessionFile(parts[4])
	}
	return false
}

// legacySessionFile is a session file, its lock, or what a crashed atomic
// write of a session file left behind (".<id>.jsonl.<random>").
func legacySessionFile(name string) bool {
	return name == ".lock" || strings.HasSuffix(name, ".jsonl") ||
		strings.HasPrefix(name, ".") && strings.Contains(name, ".jsonl.")
}

// currentSessionPath matches sessions/.lock and sessions/v3/<id>.jsonl.
func currentSessionPath(parts []string) bool {
	return len(parts) == 2 && parts[0] == "sessions" && parts[1] == ".lock" ||
		len(parts) == 3 && parts[0] == "sessions" && parts[1] == "v3" && strings.HasSuffix(parts[2], ".jsonl")
}
