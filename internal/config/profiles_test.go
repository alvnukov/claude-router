package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// profileFixture writes a one-provider setup with an opus family route to a
// temp providers.json and returns a store over it and the number of times the
// store has run its profile hook.
func profileFixture(t *testing.T) (*Store, *int, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ROUTER_ENV_FILE", filepath.Join(dir, "env"))
	path := filepath.Join(dir, "providers.json")
	c := Config{Local: oneProvider("http://example.test/v1", "a", "b"), FirstByte: 45 * time.Second}
	c.Local.FamilyRoutes = map[string]map[string]Route{"opus": {"high": {Mode: "model", Model: "p/a"}}}
	if err := WriteProviders(path, c.Local); err != nil {
		t.Fatal(err)
	}
	s := NewStore(c, path)
	hooks := new(int)
	s.OnProfileChange(func() { *hooks++ })
	return s, hooks, path
}

func TestProfileStaleSnapshotCannotReactivateOldProfile(t *testing.T) {
	s, _, _ := profileFixture(t)
	if err := s.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	stale := s.Get().Local.Clone()
	if err := s.ActivateProfile("cloud"); err != nil {
		t.Fatal(err)
	}
	stale.FamilyRoutes["opus"]["high"] = Route{Mode: "disabled"}
	if err := s.Update(Replace(stale, "")); err == nil || s.Get().Local.ActiveProfile != "cloud" {
		t.Fatalf("stale edit changed active profile: %v", err)
	}
}

func TestProfilesActivationWritesOnlyPointer(t *testing.T) {
	s, _, path := profileFixture(t)
	if err := s.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	profilePath := path + ".profiles/default.json"
	old, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(before, []byte(`"profiles"`)) || bytes.Contains(before, []byte(`"active_profile"`)) {
		t.Fatalf("globals include profiles or pointer: %s", before)
	}
	if err := s.ActivateProfile("cloud"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	still, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	pointer, err := os.ReadFile(path + ".active-profile")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !bytes.Equal(old, still) || !strings.Contains(string(pointer), "cloud") {
		t.Fatal("activation rewrote definitions or omitted pointer")
	}
}

func TestCorruptExistingProvidersDoesNotFallbackToEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, []byte("{invalid-json"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := startup(path, false)
	if err == nil {
		t.Fatal("corrupt existing providers accepted")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || string(after) != "{invalid-json" {
		t.Fatal("corrupt config overwritten")
	}
	if _, err := os.Stat(path + ".before-profiles"); !os.IsNotExist(err) {
		t.Fatal("backup created from corrupt config")
	}
}

func TestProfileMigrationSecondStartupPreservesFilesAndBackup(t *testing.T) {
	s, _, path := profileFixture(t)
	if err := s.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	paths := []string{path, path + ".active-profile", path + ".profiles/default.json", path + ".before-profiles"}
	before := make([][]byte, len(paths))
	for i, p := range paths {
		var err error
		before[i], err = os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
	}
	reloaded, err := ReadProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewStore(Config{Local: reloaded}, path)
	if err := restarted.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	for i, p := range paths {
		after, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(before[i], after) {
			t.Fatalf("second startup changed %s: %v", p, err)
		}
	}
}

func TestProfilePointerFailedWritePreservesOldValue(t *testing.T) {
	s, _, path := profileFixture(t)
	if err := s.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	pointer := path + ".active-profile"
	original, err := os.ReadFile(pointer)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(pointer+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateProfile("cloud"); err == nil {
		t.Fatal("expected failed pointer write")
	}
	current, err := os.ReadFile(pointer)
	if err != nil || !bytes.Equal(original, current) || s.Get().Local.ActiveProfile != "default" {
		t.Fatal("failed activation changed pointer or memory")
	}
}

func TestProfileFileFailedWritePreservesOldValue(t *testing.T) {
	s, _, path := profileFixture(t)
	if err := s.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	profile := path + ".profiles/default.json"
	original, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(profile+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	l := s.Get().Local.Clone()
	l.FamilyRoutes["opus"]["high"] = Route{Mode: "disabled"}
	if err := s.Update(Replace(l, "")); err == nil {
		t.Fatal("expected failed profile write")
	}
	current, err := os.ReadFile(profile)
	if err != nil || !bytes.Equal(original, current) {
		t.Fatal("failed write changed profile")
	}
}

func TestProfilePointerHandEditIsWatched(t *testing.T) {
	s, hooks, path := profileFixture(t)
	if err := s.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	before := *hooks
	if err := writeActiveProfile(path, "cloud"); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if s.Get().Local.ActiveProfile != "cloud" || *hooks != before+1 {
		t.Fatalf("hand edit did not switch and run the profile hook once: %d runs", *hooks-before)
	}
}

func TestInlineProfilesMigrateToSeparateFiles(t *testing.T) {
	s, _, path := profileFixture(t)
	l := s.Get().Local.Clone()
	l.ActiveProfile = "default"
	l.Profiles = map[string]Profile{"default": l.routing(), "cloud": l.routing()}
	inline, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, inline, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewStore(Config{Local: loaded}, path)
	if err := restarted.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(disk, []byte(`"profiles"`)) {
		t.Fatal("inline profile bodies remained")
	}
	if _, err := os.Stat(path + ".profiles/cloud.json"); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".before-profiles")
	if err != nil || !bytes.Equal(backup, inline) {
		t.Fatal("old inline layout not backed up")
	}
}

func TestProfileFailedGlobalWriteKeepsActivePointer(t *testing.T) {
	s, _, path := profileFixture(t)
	if err := s.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	pointer, err := os.ReadFile(path + ".active-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	l := s.Get().Local.Clone()
	l.Catalog.Anthropic = []string{"claude-opus-5"}
	if err := s.Update(Replace(l, "")); err == nil {
		t.Fatal("expected failed global write")
	}
	after, err := os.ReadFile(path + ".active-profile")
	if err != nil || !bytes.Equal(pointer, after) {
		t.Fatal("failed write changed active pointer")
	}
}

func TestProfileDeleteRemovesFileForRestart(t *testing.T) {
	s, _, path := profileFixture(t)
	if err := s.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProfile("cloud"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".profiles/cloud.json"); !os.IsNotExist(err) {
		t.Fatalf("deleted profile still on disk: %v", err)
	}
	loaded, err := ReadProviders(path)
	if err != nil || len(loaded.Profiles) != 1 {
		t.Fatalf("deleted profile returned on restart: %v %+v", err, loaded.Profiles)
	}
}

func TestMissingProfileDirectoryDoesNotFallbackToEnv(t *testing.T) {
	s, _, path := profileFixture(t)
	if err := s.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".profiles", path+".profiles.hidden"); err != nil {
		t.Fatal(err)
	}
	if _, err := startup(path, false); err == nil {
		t.Fatal("missing profile directory accepted as missing providers file")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("damaged profile layout overwrote globals")
	}
}

func TestFormProfileGuardUsesSubmittedNameUnderLock(t *testing.T) {
	s, _, _ := profileFixture(t)
	if err := s.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateProfile("cloud"); err != nil {
		t.Fatal(err)
	}
	// Simulates activation between the handler's initial check and its snapshot.
	current := s.Get().Local.Clone()
	current.FamilyRoutes["opus"] = map[string]Route{"high": {Mode: "anthropic"}}
	if err := s.Update(Replace(current, "default")); err == nil {
		t.Fatal("form from default edited cloud after activation")
	}
	if s.Get().Local.FamilyRoutes["opus"]["high"].Mode == "anthropic" {
		t.Fatal("stale form changed active route")
	}
}

func TestInterruptedProfileMigrationDoesNotReplaceSavedRoutes(t *testing.T) {
	s, _, path := profileFixture(t)
	original := s.Get().Local.FamilyRoutes["opus"]["high"]
	if err := os.Mkdir(path+".active-profile.tmp", 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureProfiles(); err == nil {
		t.Fatal("expected pointer write failure")
	}
	saved, err := os.ReadFile(path + ".profiles/default.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadLocal(path); err == nil {
		t.Fatal("interrupted migration loaded as legacy configuration")
	}
	if _, err := startup(path, false); err == nil {
		t.Fatal("interrupted migration started")
	}
	after, err := os.ReadFile(path + ".profiles/default.json")
	if err != nil || !bytes.Equal(saved, after) {
		t.Fatal("interrupted migration overwrote saved profile")
	}
	var profile Profile
	if err := json.Unmarshal(after, &profile); err != nil || profile.FamilyRoutes["opus"]["high"] != original {
		t.Fatalf("old route lost: %v %+v", err, profile)
	}
}

// Reload must finish applying the snapshot it read before activation can write
// a new pointer. Otherwise an older reload can undo the activation in memory.
func TestReloadReadBeforeActivationKeepsNewProfile(t *testing.T) {
	s, hooks, path := profileFixture(t)
	if err := s.EnsureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	before := *hooks
	readOld, release := make(chan struct{}), make(chan struct{})
	reloadDone := make(chan error, 1)
	go func() {
		reloadDone <- s.reloadProfilesFrom(func(path string) (Local, error) {
			l, err := ReadProviders(path)
			close(readOld)
			<-release
			return l, err
		})
	}()
	<-readOld
	activated := make(chan error, 1)
	activationStarted := make(chan struct{})
	go func() {
		close(activationStarted)
		activated <- s.ActivateProfile("cloud")
	}()
	<-activationStarted
	close(release)
	if err := <-reloadDone; err != nil {
		t.Fatal(err)
	}
	if err := <-activated; err != nil {
		t.Fatal(err)
	}
	disk, err := ReadProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	if disk.ActiveProfile != "cloud" || s.Get().Local.ActiveProfile != "cloud" || s.Get().RouteFor("claude-opus-5", "high").Mode != "disabled" || *hooks != before+1 {
		t.Fatalf("activation lost: disk=%s memory=%s route=%+v hooks=%d", disk.ActiveProfile, s.Get().Local.ActiveProfile, s.Get().RouteFor("claude-opus-5", "high"), *hooks-before)
	}
}
