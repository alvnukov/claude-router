package privacy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"localrouter/internal/platform"
)

type mapRecord struct {
	Kind        Kind      `json:"kind"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Real        string    `json:"real,omitempty"`
	Pseudo      string    `json:"pseudo,omitempty"`
	At          time.Time `json:"at"`
}
type keyRecord struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
}
type sessionData struct {
	key          []byte
	version      int
	entries      map[string]mapRecord
	fingerprints map[string]bool
	pending      []mapRecord
	validBytes   int64
}

func entityKey(kind Kind, real string) string { return string(kind) + "\x00" + real }

func (s *sessionData) add(r mapRecord) {
	if _, ok := s.entries[entityKey(r.Kind, r.Real)]; !ok {
		s.entries[entityKey(r.Kind, r.Real)] = r
		s.pending = append(s.pending, r)
	}
}

type sharedSessions struct {
	sync.Mutex
	live map[string]int
}

var sessionLocks sync.Map
var sessionIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type sessionStore struct {
	dir    string
	now    func() time.Time
	rules  *Rules
	shared *sharedSessions
	newKey func() ([]byte, error)
}
type sessionView struct {
	session *sessionData
	all     map[string]*sessionData
	hold    func() func()
}

// Session files of PRF version 1 stay in the sessions directory, which the
// code before versions reads; version 2 files live in its v2 subdirectory,
// which that code skips. A binary rolled back below version 2 therefore starts
// such a session with a fresh key instead of reading it with the old PRF.
const (
	prfV1 = 1
	prfV2 = 2
)

func (s *sessionStore) versionDir(version int) string {
	if version == prfV1 {
		return s.dir
	}
	return filepath.Join(s.dir, "v2")
}

func randomKey() ([]byte, error) { b := make([]byte, 32); _, err := rand.Read(b); return b, err }
func newSessionStore(home string, now func() time.Time) *sessionStore {
	dir, _ := filepath.Abs(filepath.Join(home, "privacy", "sessions"))
	shared, _ := sessionLocks.LoadOrStore(dir, &sharedSessions{live: make(map[string]int)})
	return &sessionStore{dir: dir, now: now, shared: shared.(*sharedSessions), newKey: randomKey}
}
func (s *sessionStore) readAll() (map[string]*sessionData, error) {
	all := make(map[string]*sessionData)
	pseudos := make(map[string]mapRecord)
	for _, version := range []int{prfV1, prfV2} {
		if err := s.readDir(version, all, pseudos); err != nil {
			return nil, err
		}
	}
	return all, nil
}
func (s *sessionStore) readDir(version int, all map[string]*sessionData, pseudos map[string]mapRecord) error {
	dir := s.versionDir(version)
	files, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(f.Name(), ".jsonl")
		if !sessionIDRE.MatchString(id) {
			return errors.New("privacy: invalid session filename")
		}
		path := filepath.Join(dir, f.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("privacy: session is not a regular file")
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return errors.New("privacy: session file permissions must be 0600")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sd, err := readSession(data)
		if err != nil {
			return fmt.Errorf("privacy: session %s: %w", id, err)
		}
		for _, r := range sd.entries {
			p := strings.ToLower(r.Pseudo)
			if issued, ok := pseudos[p]; ok {
				if issued.Kind != r.Kind || issued.Real != r.Real || issued.Pseudo != r.Pseudo || explicitPseudonym(s.rules, r.Kind, r.Real) != r.Pseudo {
					return errors.New("privacy: duplicate pseudonym in session index")
				}
			}
			pseudos[p] = r
		}
		if _, ok := all[id]; ok {
			return fmt.Errorf("privacy: session %s exists in two versions", id)
		}
		sd.version = version
		all[id] = sd
	}
	return nil
}
func readSession(data []byte) (*sessionData, error) {
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return nil, errors.New("missing session key")
	}
	lines := bytes.Split(data[:end], []byte{'\n'})
	var kr keyRecord
	if err := json.Unmarshal(lines[0], &kr); err != nil || kr.Kind != "key" {
		return nil, errors.New("missing session key")
	}
	key, err := hex.DecodeString(kr.Key)
	if err != nil || len(key) != 32 {
		return nil, errors.New("invalid session key")
	}
	sd := &sessionData{key: key, entries: make(map[string]mapRecord), validBytes: int64(end + 1)}
	seen := make(map[string]bool)
	for _, line := range lines[1:] {
		var r mapRecord
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, errors.New("invalid session mapping")
		}
		if r.Kind == kindFingerprint {
			digest, err := hex.DecodeString(r.Fingerprint)
			if err != nil || len(digest) != 32 || r.Real != "" || r.Pseudo != "" {
				return nil, errors.New("invalid secret fingerprint")
			}
			if sd.fingerprints == nil {
				sd.fingerprints = make(map[string]bool)
			}
			sd.fingerprints[r.Fingerprint] = true
			continue
		}
		if r.Real == "" || r.Pseudo == "" || !mappedKind(r.Kind) {
			return nil, errors.New("invalid session mapping")
		}
		if _, ok := sd.entries[entityKey(r.Kind, r.Real)]; ok || seen[strings.ToLower(r.Pseudo)] {
			return nil, errors.New("duplicate session mapping")
		}
		sd.entries[entityKey(r.Kind, r.Real)] = r
		seen[strings.ToLower(r.Pseudo)] = true
	}
	return sd, nil
}
func mappedKind(k Kind) bool {
	switch k {
	case KindLogin, KindHost, KindEmail, KindPhone, KindPerson, KindOrg, KindUnit, KindProject, KindAddress:
		return true
	}
	return false
}
func (s *sessionStore) locked(fn func() error) error {
	s.shared.Lock()
	defer s.shared.Unlock()
	for _, dir := range []string{filepath.Dir(s.dir), s.dir, s.versionDir(prfV2)} {
		if info, err := os.Lstat(dir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return errors.New("privacy: invalid session directory")
		}
		if err := platform.MkdirPrivate(dir); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	if info, err := os.Lstat(filepath.Join(s.dir, ".lock")); err == nil && !info.Mode().IsRegular() {
		return errors.New("privacy: invalid lock file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return platform.WithLock(ctx, filepath.Join(s.dir, ".lock"), fn)
}
func (s *sessionStore) transaction(id string, resume bool, fn func(*sessionView) error) error {
	if !sessionIDRE.MatchString(id) {
		return errors.New("privacy: invalid session id")
	}
	return s.locked(func() error {
		all, err := s.readAll()
		if err != nil {
			return err
		}
		sd := all[id]
		fresh := sd == nil
		if fresh {
			if resume {
				return &RejectError{Reason: "словарь сессии отсутствует; начните новую сессию"}
			}
			key, err := s.newKey()
			if err != nil {
				return err
			}
			if len(key) != 32 {
				return errors.New("privacy: invalid generated key")
			}
			sd = &sessionData{key: key, version: prfV2, entries: make(map[string]mapRecord)}
			all[id] = sd
		}
		hold := func() func() {
			s.shared.live[id]++
			var once sync.Once
			return func() {
				once.Do(func() {
					s.shared.Lock()
					defer s.shared.Unlock()
					s.shared.live[id]--
					if s.shared.live[id] == 0 {
						delete(s.shared.live, id)
					}
				})
			}
		}
		if err := fn(&sessionView{sd, all, hold}); err != nil {
			return err
		}
		path := filepath.Join(s.versionDir(sd.version), id+".jsonl")
		if fresh {
			line, _ := json.Marshal(keyRecord{"key", hex.EncodeToString(sd.key)})
			line = append(line, '\n')
			if err := platform.WriteFileAtomic(path, line, 0o600); err != nil {
				return err
			}
			sd.validBytes = int64(len(line))
		}
		if len(sd.pending) > 0 {
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return err
			}
			err = f.Truncate(sd.validBytes)
			if err == nil {
				for _, r := range sd.pending {
					line, _ := json.Marshal(r)
					if _, err = f.Write(append(line, '\n')); err != nil {
						break
					}
				}
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
		}
		now := s.now()
		return os.Chtimes(path, now, now)
	})
}
func (s *sessionStore) prune(now time.Time, days int) (int, error) {
	// An unused engine and a request-only scope must create no files.
	if _, err := os.Stat(s.dir); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	removed := 0
	err := s.locked(func() error {
		for _, version := range []int{prfV1, prfV2} {
			dir := s.versionDir(version)
			files, err := os.ReadDir(dir)
			if err != nil {
				return err
			}
			for _, f := range files {
				id := strings.TrimSuffix(f.Name(), ".jsonl")
				if id == f.Name() || s.shared.live[id] > 0 {
					continue
				}
				info, err := f.Info()
				if err != nil {
					return err
				}
				if info.ModTime().Before(now.AddDate(0, 0, -days)) {
					if err := os.Remove(filepath.Join(dir, f.Name())); err != nil {
						return err
					}
					removed++
				}
			}
		}
		return nil
	})
	return removed, err
}
func (s *sessionStore) forget(id string) error {
	if id != "" && !sessionIDRE.MatchString(id) {
		return &RejectError{Reason: "invalid session id"}
	}
	if _, err := os.Stat(s.dir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return s.locked(func() error {
		for _, version := range []int{prfV1, prfV2} {
			dir := s.versionDir(version)
			files, err := os.ReadDir(dir)
			if err != nil {
				return err
			}
			for _, f := range files {
				if strings.HasSuffix(f.Name(), ".jsonl") && (id == "" || f.Name() == id+".jsonl") {
					if err := os.Remove(filepath.Join(dir, f.Name())); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}
