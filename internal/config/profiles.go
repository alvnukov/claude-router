package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A profile owns routing decisions, never credentials or the global model catalog.
// ActiveProfile is a pointer separate from the profile definitions in providers.json.
type Profile struct {
	FamilyRoutes map[string]map[string]Route `json:"family_routes"`
	Routes       map[string]map[string]Route `json:"routes"`
	ModelPools   map[string][]PoolTarget     `json:"model_pools"`
	PoolSettings map[string]PoolSettings     `json:"pool_settings,omitempty"`
}

func (l Local) routing() Profile {
	c := l.cloneRouting()
	return Profile{FamilyRoutes: c.FamilyRoutes, Routes: c.Routes, ModelPools: c.ModelPools, PoolSettings: c.PoolSettings}
}

func (l Local) cloneRouting() Local {
	c := Local{FamilyRoutes: l.FamilyRoutes, Routes: l.Routes, ModelPools: l.ModelPools, PoolSettings: l.PoolSettings}
	return c.Clone()
}

func (l *Local) UseProfile(name string) error {
	p, ok := l.Profiles[name]
	if !ok {
		return fmt.Errorf("профиль %q не найден", name)
	}
	c := Local{FamilyRoutes: p.FamilyRoutes, Routes: p.Routes, ModelPools: p.ModelPools, PoolSettings: p.PoolSettings}.Clone()
	l.FamilyRoutes, l.Routes, l.ModelPools, l.PoolSettings = c.FamilyRoutes, c.Routes, c.ModelPools, c.PoolSettings
	l.ActiveProfile = name
	return nil
}

func (l *Local) syncActiveProfile() error {
	if l.Profiles == nil {
		return nil
	}
	if _, ok := l.Profiles[l.ActiveProfile]; !ok {
		return fmt.Errorf("активный профиль %q не найден", l.ActiveProfile)
	}
	l.Profiles[l.ActiveProfile] = l.routing()
	return nil
}

func (l *Local) validateProfiles() error {
	if l.Profiles == nil {
		return l.Validate()
	}
	if len(l.Profiles) == 0 {
		return fmt.Errorf("нужен хотя бы один профиль")
	}
	if _, ok := l.Profiles[l.ActiveProfile]; !ok {
		return fmt.Errorf("активный профиль %q не найден", l.ActiveProfile)
	}
	if err := l.Validate(); err != nil {
		return fmt.Errorf("профиль %q: %w", l.ActiveProfile, err)
	}
	if err := l.syncActiveProfile(); err != nil {
		return err
	}
	for name := range l.Profiles {
		if !profileNameOK(name) {
			return fmt.Errorf("неверное имя профиля %q", name)
		}
		candidate := l.Clone()
		if err := candidate.UseProfile(name); err != nil {
			return err
		}
		if err := candidate.Validate(); err != nil {
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
func (l *Local) repairInactiveProfiles() {
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
		c := Local{FamilyRoutes: profile.FamilyRoutes, Routes: profile.Routes, ModelPools: profile.ModelPools, PoolSettings: profile.PoolSettings}.Clone()
		for poolName, members := range c.ModelPools {
			kept := make([]PoolTarget, 0, len(members))
			for _, member := range members {
				if known[member.Model] {
					kept = append(kept, member)
				}
			}
			c.ModelPools[poolName] = kept
		}
		for _, rules := range c.AllRouteRules() {
			for effort, route := range rules {
				if route.Mode == "model" && !known[route.Model] {
					rules[effort] = Route{Mode: "disabled"}
				}
			}
		}
		l.Profiles[name] = c.routing()
	}
}

func (l *Local) RenameInactiveProvider(oldName, newName string) {
	for name, profile := range l.Profiles {
		if name == l.ActiveProfile {
			continue
		}
		c := Local{FamilyRoutes: profile.FamilyRoutes, Routes: profile.Routes, ModelPools: profile.ModelPools, PoolSettings: profile.PoolSettings}.Clone()
		for poolName, members := range c.ModelPools {
			for i := range members {
				if strings.HasPrefix(members[i].Model, oldName+"/") {
					members[i].Model = newName + strings.TrimPrefix(members[i].Model, oldName)
				}
			}
			c.ModelPools[poolName] = members
		}
		for _, rules := range c.AllRouteRules() {
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

func (s *Store) EnsureProfiles() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c.Local.Profiles != nil {
		if s.provPath == "" {
			return nil
		}
		if _, err := os.Stat(s.provPath + ".active-profile"); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		return s.update(".before-profiles", func(*Local) error { return nil })
	}
	add := func(l *Local) error {
		l.Profiles = map[string]Profile{"default": l.routing()}
		l.ActiveProfile = "default"
		return nil
	}
	if s.provPath != "" {
		return s.update(".before-profiles", add)
	}
	// Without a providers file the profile lives in memory only.
	next, err := s.prepare(add)
	if err != nil {
		return err
	}
	s.c = next
	return nil
}

func (s *Store) CreateProfile(name string, clone bool) error {
	if !profileNameOK(name) {
		return fmt.Errorf("неверное имя профиля %q", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update("", func(l *Local) error {
		if l.Profiles == nil {
			return fmt.Errorf("профили не инициализированы")
		}
		if _, exists := l.Profiles[name]; exists {
			return fmt.Errorf("профиль %q уже существует", name)
		}
		p := Profile{FamilyRoutes: map[string]map[string]Route{}, Routes: map[string]map[string]Route{}, ModelPools: map[string][]PoolTarget{}}
		if clone {
			p = l.routing()
		}
		l.Profiles[name] = p
		if s.provPath == "" {
			return fmt.Errorf("файл провайдеров отключён")
		}
		return nil
	})
}

func (s *Store) ActivateProfile(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.c.Local.Clone()
	if l.Profiles == nil {
		return fmt.Errorf("профили не инициализированы")
	}
	if name == l.ActiveProfile {
		return nil
	}
	if err := l.UseProfile(name); err != nil {
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
	s.commit(kindProfiles)
	s.c.Local = l
	if s.onProfileChange != nil {
		s.onProfileChange()
	}
	return nil
}

func (s *Store) DeleteProfile(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.c.Local.Clone()
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
	s.c.Local = l
	s.commit(kindProfiles)
	return nil
}

// A hand edit can still be loaded through the existing mtime watcher.
func (s *Store) Reload() error {
	return s.reloadProfilesFrom(ReadProviders)
}

func (s *Store) reloadProfilesFrom(read func(string) (Local, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := read(s.provPath)
	if err != nil {
		return err
	}
	before := s.c.Local.ActiveProfile
	next, err := s.prepare(func(cur *Local) error {
		*cur = l
		return nil
	})
	if err != nil {
		return err
	}
	s.c = next
	if s.onProfileChange != nil && before != l.ActiveProfile {
		s.onProfileChange()
	}
	return nil
}
