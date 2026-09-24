package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A profile owns routing decisions, never credentials or the global model catalog.
// ActiveProfile is a pointer separate from the profile definitions in providers.json.
type routingProfile struct {
	FamilyRoutes map[string]map[string]modelRoute `json:"family_routes"`
	Routes       map[string]map[string]modelRoute `json:"routes"`
	ModelPools   map[string][]poolTarget          `json:"model_pools"`
	PoolSettings map[string]poolSettings          `json:"pool_settings,omitempty"`
}

func (l localSetup) routing() routingProfile {
	c := l.cloneRouting()
	return routingProfile{FamilyRoutes: c.FamilyRoutes, Routes: c.Routes, ModelPools: c.ModelPools, PoolSettings: c.PoolSettings}
}

func (l localSetup) cloneRouting() localSetup {
	c := localSetup{FamilyRoutes: l.FamilyRoutes, Routes: l.Routes, ModelPools: l.ModelPools, PoolSettings: l.PoolSettings}
	return c.clone()
}

func (l *localSetup) useProfile(name string) error {
	p, ok := l.Profiles[name]
	if !ok {
		return fmt.Errorf("профиль %q не найден", name)
	}
	c := localSetup{FamilyRoutes: p.FamilyRoutes, Routes: p.Routes, ModelPools: p.ModelPools, PoolSettings: p.PoolSettings}.clone()
	l.FamilyRoutes, l.Routes, l.ModelPools, l.PoolSettings = c.FamilyRoutes, c.Routes, c.ModelPools, c.PoolSettings
	l.ActiveProfile = name
	return nil
}

func (l *localSetup) syncActiveProfile() error {
	if l.Profiles == nil {
		return nil
	}
	if _, ok := l.Profiles[l.ActiveProfile]; !ok {
		return fmt.Errorf("активный профиль %q не найден", l.ActiveProfile)
	}
	l.Profiles[l.ActiveProfile] = l.routing()
	return nil
}

func (l *localSetup) validateProfiles() error {
	if l.Profiles == nil {
		return l.validate()
	}
	if len(l.Profiles) == 0 {
		return fmt.Errorf("нужен хотя бы один профиль")
	}
	if _, ok := l.Profiles[l.ActiveProfile]; !ok {
		return fmt.Errorf("активный профиль %q не найден", l.ActiveProfile)
	}
	if err := l.validate(); err != nil {
		return fmt.Errorf("профиль %q: %w", l.ActiveProfile, err)
	}
	if err := l.syncActiveProfile(); err != nil {
		return err
	}
	for name := range l.Profiles {
		if !profileNameOK(name) {
			return fmt.Errorf("неверное имя профиля %q", name)
		}
		candidate := l.clone()
		if err := candidate.useProfile(name); err != nil {
			return err
		}
		if err := candidate.validate(); err != nil {
			return fmt.Errorf("профиль %q: %w", name, err)
		}
	}
	return nil
}

func profileNameOK(name string) bool {
	return name != "" && len(name) <= 80 && name == strings.TrimSpace(name) && !strings.ContainsAny(name, "/\\\r\n\t") && name != "." && name != ".."
}

// Removing a global model also removes stale references in inactive profiles.
// The active profile is edited by the caller and validated normally.
func (l *localSetup) repairInactiveProfiles() {
	if l.Profiles == nil {
		return
	}
	known := map[string]bool{}
	for _, model := range l.Models {
		known[model.Key()] = true
	}
	for name, profile := range l.Profiles {
		if name == l.ActiveProfile {
			continue
		}
		c := localSetup{FamilyRoutes: profile.FamilyRoutes, Routes: profile.Routes, ModelPools: profile.ModelPools, PoolSettings: profile.PoolSettings}.clone()
		for poolName, members := range c.ModelPools {
			kept := make([]poolTarget, 0, len(members))
			for _, member := range members {
				if known[member.Model] {
					kept = append(kept, member)
				}
			}
			c.ModelPools[poolName] = kept
		}
		for _, rules := range c.allRouteRules() {
			for effort, route := range rules {
				if route.Mode == "model" && !known[route.Model] {
					rules[effort] = modelRoute{Mode: "disabled"}
				}
			}
		}
		l.Profiles[name] = c.routing()
	}
}

func (l *localSetup) renameInactiveProvider(oldName, newName string) {
	for name, profile := range l.Profiles {
		if name == l.ActiveProfile {
			continue
		}
		c := localSetup{FamilyRoutes: profile.FamilyRoutes, Routes: profile.Routes, ModelPools: profile.ModelPools, PoolSettings: profile.PoolSettings}.clone()
		for poolName, members := range c.ModelPools {
			for i := range members {
				if strings.HasPrefix(members[i].Model, oldName+"/") {
					members[i].Model = newName + strings.TrimPrefix(members[i].Model, oldName)
				}
			}
			c.ModelPools[poolName] = members
		}
		for _, rules := range c.allRouteRules() {
			for effort, route := range rules {
				if route.Mode == "model" && strings.HasPrefix(route.Model, oldName+"/") {
					route.Model = newName + strings.TrimPrefix(route.Model, oldName)
					rules[effort] = route
				}
			}
		}
		l.Profiles[name] = c.routing()
	}
}

func profileFile(path, name string) string { return filepath.Join(path+".profiles", name+".json") }

// Track the pointer and profile definitions independently of providers.json.
func profilesMtime(path string) time.Time {
	latest := mtime(path + ".active-profile")
	entries, err := os.ReadDir(path + ".profiles")
	if err != nil {
		return latest
	}
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest
}

func (s *configStore) ensureProfiles() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c.local.Profiles != nil {
		if s.provPath == "" {
			return nil
		}
		if _, err := os.Stat(s.provPath + ".active-profile"); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := saveConfigurationMigration(s.provPath, s.c.local, ".before-profiles"); err != nil {
			return err
		}
		s.provMtime, s.profileMtime = mtime(s.provPath), profilesMtime(s.provPath)
		return nil
	}
	l := s.c.local.clone()
	l.Profiles = map[string]routingProfile{"default": l.routing()}
	l.ActiveProfile = "default"
	if err := l.validateProfiles(); err != nil {
		return err
	}
	if s.provPath != "" {
		if err := saveConfigurationMigration(s.provPath, l, ".before-profiles"); err != nil {
			return err
		}
		s.provMtime = mtime(s.provPath)
		s.profileMtime = profilesMtime(s.provPath)
	}
	s.c.local = l
	return nil
}

func (s *configStore) createProfile(name string, clone bool) error {
	if !profileNameOK(name) {
		return fmt.Errorf("неверное имя профиля %q", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.c.local.clone()
	if l.Profiles == nil {
		return fmt.Errorf("профили не инициализированы")
	}
	if _, exists := l.Profiles[name]; exists {
		return fmt.Errorf("профиль %q уже существует", name)
	}
	p := routingProfile{FamilyRoutes: map[string]map[string]modelRoute{}, Routes: map[string]map[string]modelRoute{}, ModelPools: map[string][]poolTarget{}}
	if clone {
		p = l.routing()
	}
	l.Profiles[name] = p
	if err := l.validateProfiles(); err != nil {
		return err
	}
	if err := s.persistLocalLocked(l); err != nil {
		return err
	}
	s.c.local = l
	return nil
}

func (s *configStore) activateProfile(name string, h *health) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.c.local.clone()
	if l.Profiles == nil {
		return fmt.Errorf("профили не инициализированы")
	}
	if name == l.ActiveProfile {
		return nil
	}
	if err := l.useProfile(name); err != nil {
		return err
	}
	if err := l.validateProfiles(); err != nil {
		return err
	}
	if s.provPath == "" {
		return fmt.Errorf("файл провайдеров отключён")
	}
	if err := writeActiveProfile(s.provPath, name); err != nil {
		return err
	}
	s.profileMtime = profilesMtime(s.provPath)
	s.c.local = l
	if h != nil {
		h.clearSessions()
	}
	return nil
}

func (s *configStore) deleteProfile(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.c.local.clone()
	if _, ok := l.Profiles[name]; !ok {
		return fmt.Errorf("профиль %q не найден", name)
	}
	if len(l.Profiles) <= 1 {
		return fmt.Errorf("нельзя удалить последний профиль")
	}
	if name == l.ActiveProfile {
		return fmt.Errorf("сначала переключите активный профиль")
	}
	delete(l.Profiles, name)
	if err := os.Remove(profileFile(s.provPath, name)); err != nil {
		return err
	}
	s.c.local = l
	s.profileMtime = profilesMtime(s.provPath)
	return nil
}

func (s *configStore) persistLocalLocked(l localSetup) error {
	if s.provPath == "" {
		return fmt.Errorf("файл провайдеров отключён")
	}
	if err := writeProviders(s.provPath, l); err != nil {
		return err
	}
	s.provMtime = mtime(s.provPath)
	s.profileMtime = profilesMtime(s.provPath)
	return nil
}

// A hand edit can still be loaded through the existing mtime watcher.
func (s *configStore) reloadProfiles(h *health) error {
	l, err := readProviders(s.provPath)
	if err != nil {
		return err
	}
	before := s.get().local.ActiveProfile
	if err := s.applyLocal(l, false); err != nil {
		return err
	}
	if h != nil && before != l.ActiveProfile {
		h.clearSessions()
	}
	return nil
}
