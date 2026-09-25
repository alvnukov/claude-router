package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfileStaleSnapshotCannotReactivateOldProfile(t *testing.T) {
	cs, h, _ := profileFixture(t)
	if err := cs.ensureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.createProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	stale := cs.get().local.clone()
	if err := cs.activateProfile("cloud", h); err != nil {
		t.Fatal(err)
	}
	stale.FamilyRoutes["opus"]["high"] = modelRoute{Mode: "disabled"}
	if err := cs.applyLocal(stale, true); err == nil || cs.get().local.ActiveProfile != "cloud" {
		t.Fatalf("stale edit changed active profile: %v", err)
	}
}

func TestProfilesActivationWritesOnlyPointer(t *testing.T) {
	cs, h, path := profileFixture(t)
	if err := cs.ensureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.createProfile("cloud", false); err != nil {
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
	if err := cs.activateProfile("cloud", h); err != nil {
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
	t.Setenv("ROUTER_PROVIDERS_FILE", path)
	_, err := loadConfigChecked()
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
	cs, _, path := profileFixture(t)
	if err := cs.ensureProfiles(); err != nil {
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
	reloaded, err := readProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newConfigStore(config{local: reloaded}, path)
	if err := restarted.ensureProfiles(); err != nil {
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
	cs, h, path := profileFixture(t)
	if err := cs.ensureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.createProfile("cloud", false); err != nil {
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
	if err := cs.activateProfile("cloud", h); err == nil {
		t.Fatal("expected failed pointer write")
	}
	current, err := os.ReadFile(pointer)
	if err != nil || !bytes.Equal(original, current) || cs.get().local.ActiveProfile != "default" {
		t.Fatal("failed activation changed pointer or memory")
	}
}

func TestProfileFileFailedWritePreservesOldValue(t *testing.T) {
	cs, _, path := profileFixture(t)
	if err := cs.ensureProfiles(); err != nil {
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
	l := cs.get().local.clone()
	l.FamilyRoutes["opus"]["high"] = modelRoute{Mode: "disabled"}
	if err := cs.applyLocal(l, true); err == nil {
		t.Fatal("expected failed profile write")
	}
	current, err := os.ReadFile(profile)
	if err != nil || !bytes.Equal(original, current) {
		t.Fatal("failed write changed profile")
	}
}

func TestProfilePointerHandEditIsWatched(t *testing.T) {
	cs, h, path := profileFixture(t)
	if err := cs.ensureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.createProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	h.bindCandidates("session", poolRoute{}, []candidate{{Key: "p/a"}})
	if err := writeActiveProfile(path, "cloud"); err != nil {
		t.Fatal(err)
	}
	if err := cs.reloadProfiles(h); err != nil {
		t.Fatal(err)
	}
	if cs.get().local.ActiveProfile != "cloud" || len(h.sessions) != 0 {
		t.Fatal("hand edit did not switch and clear sessions")
	}
}

func TestStaleRoutingFormRejectedAfterActivation(t *testing.T) {
	cs, h, _ := profileFixture(t)
	if err := cs.ensureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.createProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	u := newUIServer(newStore(10, ""), cs, h)
	if err := cs.activateProfile("cloud", h); err != nil {
		t.Fatal(err)
	}
	values := url.Values{"profile": {"default"}, "scope": {"family"}, "model": {"opus"}, "high": {"anthropic"}}
	req := httptest.NewRequest("POST", "/settings/route", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	u.handler().ServeHTTP(w, req)
	if cs.get().local.FamilyRoutes["opus"]["high"].Mode == "anthropic" {
		t.Fatal("stale routing form edited new active profile")
	}
}

func TestStalePoolSettingsFormRejectedAfterActivation(t *testing.T) {
	cs, h, _ := profileFixture(t)
	l := cs.get().local.clone()
	l.ModelPools = map[string][]poolTarget{"work": {{Model: "p/a"}}}
	l.PoolSettings = map[string]poolSettings{"work": {FirstByteSec: 2}}
	if err := cs.applyLocal(l, true); err != nil {
		t.Fatal(err)
	}
	if err := cs.ensureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.createProfile("cloud", true); err != nil {
		t.Fatal(err)
	}
	u := newUIServer(newStore(10, ""), cs, h)
	if err := cs.activateProfile("cloud", h); err != nil {
		t.Fatal(err)
	}
	values := url.Values{"profile": {"default"}, "name": {"work"}, "first_byte": {"1"}, "probe_every": {"0"}, "max_input_chars": {"0"}}
	req := httptest.NewRequest("POST", "/settings/pool-settings", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	u.handler().ServeHTTP(w, req)
	if cs.get().local.PoolSettings["work"].FirstByteSec != 2 {
		t.Fatal("stale pool settings changed new active profile")
	}
}

func TestInlineProfilesMigrateToSeparateFiles(t *testing.T) {
	cs, _, path := profileFixture(t)
	l := cs.get().local.clone()
	l.ActiveProfile = "default"
	l.Profiles = map[string]routingProfile{"default": l.routing(), "cloud": l.routing()}
	inline, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, inline, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := readProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newConfigStore(config{local: loaded}, path)
	if err := restarted.ensureProfiles(); err != nil {
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
	cs, _, path := profileFixture(t)
	if err := cs.ensureProfiles(); err != nil {
		t.Fatal(err)
	}
	pointer, err := os.ReadFile(path + ".active-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	l := cs.get().local.clone()
	l.Catalog.Anthropic = []string{"claude-opus-5"}
	if err := cs.applyLocal(l, true); err == nil {
		t.Fatal("expected failed global write")
	}
	after, err := os.ReadFile(path + ".active-profile")
	if err != nil || !bytes.Equal(pointer, after) {
		t.Fatal("failed write changed active pointer")
	}
}

func TestProfileDeleteRemovesFileForRestart(t *testing.T) {
	cs, _, path := profileFixture(t)
	if err := cs.ensureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.createProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	if err := cs.deleteProfile("cloud"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".profiles/cloud.json"); !os.IsNotExist(err) {
		t.Fatalf("deleted profile still on disk: %v", err)
	}
	loaded, err := readProviders(path)
	if err != nil || len(loaded.Profiles) != 1 {
		t.Fatalf("deleted profile returned on restart: %v %+v", err, loaded.Profiles)
	}
}

func TestMissingProfileDirectoryDoesNotFallbackToEnv(t *testing.T) {
	cs, _, path := profileFixture(t)
	if err := cs.ensureProfiles(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".profiles", path+".profiles.hidden"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROUTER_PROVIDERS_FILE", path)
	if _, err := loadConfigChecked(); err == nil {
		t.Fatal("missing profile directory accepted as missing providers file")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("damaged profile layout overwrote globals")
	}
}

func TestFormProfileGuardUsesSubmittedNameUnderLock(t *testing.T) {
	cs, h, _ := profileFixture(t)
	if err := cs.ensureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.createProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	if err := cs.activateProfile("cloud", h); err != nil {
		t.Fatal(err)
	}
	// Simulates activation between the handler's initial check and its snapshot.
	current := cs.get().local.clone()
	current.FamilyRoutes["opus"] = map[string]modelRoute{"high": {Mode: "anthropic"}}
	if err := cs.applyLocal(current, true, "default"); err == nil {
		t.Fatal("form from default edited cloud after activation")
	}
	if cs.get().local.FamilyRoutes["opus"]["high"].Mode == "anthropic" {
		t.Fatal("stale form changed active route")
	}
}

func TestInterruptedProfileMigrationDoesNotReplaceSavedRoutes(t *testing.T) {
	cs, _, path := profileFixture(t)
	original := cs.get().local.FamilyRoutes["opus"]["high"]
	if err := os.Mkdir(path+".active-profile.tmp", 0700); err != nil {
		t.Fatal(err)
	}
	if err := cs.ensureProfiles(); err == nil {
		t.Fatal("expected pointer write failure")
	}
	saved, err := os.ReadFile(path + ".profiles/default.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadLocalSetupChecked(path); err == nil {
		t.Fatal("interrupted migration loaded as legacy configuration")
	}
	t.Setenv("ROUTER_PROVIDERS_FILE", path)
	if _, err := loadConfigChecked(); err == nil {
		t.Fatal("interrupted migration started")
	}
	after, err := os.ReadFile(path + ".profiles/default.json")
	if err != nil || !bytes.Equal(saved, after) {
		t.Fatal("interrupted migration overwrote saved profile")
	}
	var profile routingProfile
	if err := json.Unmarshal(after, &profile); err != nil || profile.FamilyRoutes["opus"]["high"] != original {
		t.Fatalf("old route lost: %v %+v", err, profile)
	}
}

// Reload must finish applying the snapshot it read before activation can write
// a new pointer. Otherwise an older reload can undo the activation in memory.
func TestReloadReadBeforeActivationKeepsNewProfile(t *testing.T) {
	cs, h, path := profileFixture(t)
	if err := cs.ensureProfiles(); err != nil {
		t.Fatal(err)
	}
	if err := cs.createProfile("cloud", false); err != nil {
		t.Fatal(err)
	}
	h.bindCandidates("session", poolRoute{}, []candidate{{Key: "p/a"}})
	readOld, release := make(chan struct{}), make(chan struct{})
	reloadDone := make(chan error, 1)
	go func() {
		reloadDone <- cs.reloadProfilesFrom(h, func(path string) (localSetup, error) {
			l, err := readProviders(path)
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
		activated <- cs.activateProfile("cloud", h)
	}()
	<-activationStarted
	close(release)
	if err := <-reloadDone; err != nil {
		t.Fatal(err)
	}
	if err := <-activated; err != nil {
		t.Fatal(err)
	}
	disk, err := readProviders(path)
	if err != nil {
		t.Fatal(err)
	}
	if disk.ActiveProfile != "cloud" || cs.get().local.ActiveProfile != "cloud" || cs.get().routeFor("claude-opus-5", "high").Mode != "disabled" || len(h.sessions) != 0 {
		t.Fatalf("activation lost: disk=%s memory=%s route=%+v sessions=%d", disk.ActiveProfile, cs.get().local.ActiveProfile, cs.get().routeFor("claude-opus-5", "high"), len(h.sessions))
	}
}
