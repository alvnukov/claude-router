package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"localrouter/internal/platform"
)

const maxReplayStoreBytes = 64 << 20

// ReplayStore holds protocol state independently of the UI's bounded history.
// Each account/model/session scope has one atomic snapshot and a process-shared
// lock. Reaching a bound returns an error; old protocol state is never evicted.
type ReplayStore struct {
	dir      string
	identity os.FileInfo
}

type replaySnapshot struct {
	Version int               `json:"version"`
	Scope   string            `json:"scope"`
	Records []json.RawMessage `json:"records"`
}

func NewReplayStore(dir string) (*ReplayStore, error) {
	if dir == "" {
		return nil, errors.New("Codex replay directory is required")
	}
	path, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("Codex replay directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("Codex replay directory must not be a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("Codex replay directory: %w", err)
	}
	if err := platform.MkdirPrivate(path); err != nil {
		return nil, fmt.Errorf("create Codex replay directory: %w", err)
	}
	// Resolve ordinary parent aliases (for example macOS /var -> /private/var)
	// once, then pin the actual directory identity for the store's lifetime.
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("resolve Codex replay directory: %w", err)
	}
	path = filepath.Join(parent, filepath.Base(path))
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect Codex replay directory: %w", err)
	}
	if err := privateReplayPath(info, true); err != nil {
		return nil, err
	}
	return &ReplayStore{dir: path, identity: info}, nil
}

// Load returns a snapshot, newest first. Only an absent scope file means that
// no state has been saved; corrupt, inaccessible and oversized files are errors.
func (s *ReplayStore) Load(ctx context.Context, scope string) ([]json.RawMessage, error) {
	if err := s.validate(ctx, scope); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, fmt.Errorf("open Codex replay directory: %w", err)
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil || !os.SameFile(info, s.identity) {
		return nil, errors.New("Codex replay directory changed")
	}
	name := scope + ".json"
	info, err = root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect Codex replay: %w", err)
	}
	if err := privateReplayPath(info, false); err != nil {
		return nil, err
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open Codex replay: %w", err)
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect open Codex replay: %w", err)
	}
	if err := privateReplayPath(info, false); err != nil {
		return nil, err
	}
	if info.Size() > maxReplayStoreBytes {
		return nil, errors.New("Codex replay session exceeds 64 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxReplayStoreBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Codex replay: %w", err)
	}
	if len(data) > maxReplayStoreBytes {
		return nil, errors.New("Codex replay session exceeds 64 MiB")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var snapshot replaySnapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, errors.New("invalid Codex replay snapshot")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF || snapshot.Version != 1 || snapshot.Scope != scope || snapshot.Records == nil {
		return nil, errors.New("invalid Codex replay snapshot version, scope or records")
	}
	for _, record := range snapshot.Records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := validateStoredReplay(scope, record); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return snapshot.Records, nil
}

// Save never replaces neighbouring records, even across independent processes.
// Atomic replacement leaves the previous snapshot intact on write failure.
func (s *ReplayStore) Save(ctx context.Context, scope string, state json.RawMessage) error {
	if err := s.validate(ctx, scope); err != nil {
		return err
	}
	if err := validateStoredReplay(scope, state); err != nil {
		return err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, state); err != nil {
		return errors.New("invalid Codex replay record")
	}
	state = compact.Bytes()
	lock := filepath.Join(s.dir, scope+".lock")
	if info, err := os.Lstat(lock); err == nil {
		if err := privateReplayPath(info, false); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Codex replay lock: %w", err)
	}
	return platform.WithLock(ctx, lock, func() error {
		if err := s.validate(ctx, scope); err != nil {
			return err
		}
		records, err := s.Load(ctx, scope)
		if err != nil {
			return err
		}
		for _, record := range records {
			if bytes.Equal(record, state) {
				return nil
			}
		}
		records = append([]json.RawMessage{state}, records...)
		// This lower bound avoids allocating another oversized JSON snapshot.
		if rawItemsSize(records) > maxReplayStoreBytes {
			return errors.New("Codex replay session exceeds 64 MiB; no records were removed")
		}
		data, err := json.Marshal(replaySnapshot{Version: 1, Scope: scope, Records: records})
		if err != nil {
			return errors.New("encode Codex replay snapshot")
		}
		if len(data) > maxReplayStoreBytes {
			return errors.New("Codex replay session exceeds 64 MiB; no records were removed")
		}
		if err := s.validate(ctx, scope); err != nil {
			return err
		}
		if err := platform.WritePrivateAtomic(filepath.Join(s.dir, scope+".json"), data); err != nil {
			return fmt.Errorf("persist Codex replay: %w", err)
		}
		return nil
	})
}

func (s *ReplayStore) validate(ctx context.Context, scope string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !replayDigest(scope) {
		return errors.New("invalid Codex replay scope")
	}
	info, err := os.Lstat(s.dir)
	if err != nil {
		return fmt.Errorf("inspect Codex replay directory: %w", err)
	}
	if err := privateReplayPath(info, true); err != nil {
		return err
	}
	if !os.SameFile(info, s.identity) {
		return errors.New("Codex replay directory changed")
	}
	return nil
}

func privateReplayPath(info os.FileInfo, directory bool) error {
	if info.Mode()&os.ModeSymlink != 0 || (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
		return errors.New("Codex replay path must be a regular private file or directory, not a symlink")
	}
	mode := os.FileMode(0o600)
	if directory {
		mode = 0o700
	}
	// Windows privacy is inherited from the owned directory's ACL, as in the
	// platform package; Unix permission bits are not ACLs on Windows.
	if runtime.GOOS != "windows" && info.Mode().Perm() != mode {
		return errors.New("Codex replay path has unsafe permissions")
	}
	return nil
}

func replayDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validateStoredReplay(scope string, raw json.RawMessage) error {
	if len(raw) == 0 || len(raw) > maxProtocolBytes {
		return errors.New("invalid Codex replay record size")
	}
	var state Replay
	if json.Unmarshal(raw, &state) != nil || state.Version != 1 || state.Scope != scope || !replayDigest(state.Prefix) || state.Start < 0 || state.End < state.Start || state.Calls < 1 || len(state.ResponseIDs) != state.Calls || (state.End > state.Start && len(state.Output) == 0) || !validHeader(state.TurnState) {
		return errors.New("invalid Codex replay record version, scope or structure")
	}
	seen := make(map[string]bool, len(state.ResponseIDs))
	for _, id := range state.ResponseIDs {
		if id == "" || seen[id] {
			return errors.New("invalid Codex replay response IDs")
		}
		seen[id] = true
	}
	for _, rawItem := range state.Output {
		var item struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(rawItem, &item) != nil || item.Type == "" {
			return errors.New("invalid Codex replay output item")
		}
	}
	return nil
}
