package config

import (
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"time"
)

type CatalogModel struct {
	ID      string   `json:"id"`
	Name    string   `json:"name,omitempty"`
	Efforts []string `json:"efforts,omitempty"`
}
type ProviderCatalog struct {
	Models    []CatalogModel `json:"models"`
	UpdatedAt time.Time      `json:"updated_at"`
	Error     string         `json:"error,omitempty"`
}
type Catalog struct {
	Anthropic        []string                   `json:"anthropic,omitempty"`
	AnthropicUpdated time.Time                  `json:"anthropic_updated,omitempty"`
	Providers        map[string]ProviderCatalog `json:"providers,omitempty"`
	CodexSeen        map[string][]string        `json:"codex_seen,omitempty"`
	CheckedAt        time.Time                  `json:"checked_at,omitempty"`
	Notes            []string                   `json:"notes,omitempty"`
}

func (c Catalog) Clone() Catalog {
	c.Anthropic = append([]string(nil), c.Anthropic...)
	c.Notes = append([]string(nil), c.Notes...)
	providers := map[string]ProviderCatalog{}
	for key, value := range c.Providers {
		value.Models = append([]CatalogModel(nil), value.Models...)
		for i := range value.Models {
			value.Models[i].Efforts = append([]string(nil), value.Models[i].Efforts...)
		}
		providers[key] = value
	}
	c.Providers = providers
	seen := map[string][]string{}
	for key, ids := range c.CodexSeen {
		seen[key] = append([]string(nil), ids...)
	}
	c.CodexSeen = seen
	return c
}

// ProviderProbe is one provider's model listing, read outside the store lock.
type ProviderProbe struct {
	OK     bool
	Msg    string
	At     time.Time
	Models []CatalogModel
}

// UpdateCatalog merges network reads into the latest configuration, so an
// hourly update never overwrites a dashboard edit. snapshot is the setup the
// probes ran against: a provider edited since then keeps its old catalog.
func (s *Store) UpdateCatalog(snapshot Local, anthropic []string, anthropicErr error, probes map[string]ProviderProbe, gate Gate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gate != nil && !gate.WritesSharedState() {
		return fmt.Errorf("model catalog refresh requires active instance")
	}
	next := s.c.Local.Clone()
	next.Catalog.CheckedAt = time.Now()
	next.Catalog.Notes = nil
	if anthropicErr != nil {
		next.Catalog.Notes = append(next.Catalog.Notes, "Anthropic: "+anthropicErr.Error()+". Сохранён предыдущий каталог.")
	} else {
		next.Catalog.Anthropic = anthropic
		next.Catalog.AnthropicUpdated = time.Now()
	}
	for _, p := range next.Providers {
		res, exists := probes[p.Name]
		before, wasPresent := snapshot.Provider(p.Name)
		if !exists || !wasPresent || before != p {
			continue
		}
		cached := next.Catalog.Providers[p.Name]
		if !res.OK {
			cached.Error = res.Msg
			next.Catalog.Notes = append(next.Catalog.Notes, p.Name+": "+res.Msg+". Сохранён предыдущий каталог.")
		} else {
			cached = ProviderCatalog{UpdatedAt: res.At, Models: res.Models}
			if p.Type == "codex" {
				next.Catalog.Notes = append(next.Catalog.Notes, InheritCodexModels(&next, p.Name, cached.Models)...)
			}
		}
		next.Catalog.Providers[p.Name] = cached
	}
	next.repairInactiveProfiles()
	if err := next.validateProfiles(); err != nil {
		return err
	}
	if s.provPath != "" {
		if err := WriteProviders(s.provPath, next); err != nil {
			return err
		}
		s.wroteProviders()
	}
	s.c.Local = next
	log.Printf("model catalogs refreshed: anthropic=%d providers=%d notes=%d", len(next.Catalog.Anthropic), len(probes), len(next.Catalog.Notes))
	return nil
}

// New Codex versions append to each matching pool using that pool's newest
// earlier member as the effort template. A removed model is not re-added on
// the next refresh, and distinct named variants never inherit from each other.
func InheritCodexModels(l *Local, providerName string, models []CatalogModel) []string {
	if l.Catalog.CodexSeen == nil {
		l.Catalog.CodexSeen = map[string][]string{}
	}
	seen := map[string]bool{}
	for _, id := range l.Catalog.CodexSeen[providerName] {
		seen[id] = true
	}
	original := l.Clone()
	var notes []string
	for _, m := range models {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		key := providerName + "/" + m.ID
		if !l.HasModel(key) {
			l.Models = append(l.Models, Model{Provider: providerName, Model: m.ID})
		}
		family := CodexFamily(m.ID)
		if family == "" {
			continue
		}
		names := make([]string, 0, len(original.ModelPools))
		for name := range original.ModelPools {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			pool := original.ModelPools[name]
			var template PoolTarget
			var templateID string
			present := false
			for _, member := range l.ModelPools[name] {
				if member.Model == key {
					present = true
					break
				}
			}
			if present {
				continue
			}
			for _, member := range pool {
				prefix := providerName + "/"
				if !strings.HasPrefix(member.Model, prefix) {
					continue
				}
				id := strings.TrimPrefix(member.Model, prefix)
				if CodexFamily(id) == family && NewerModel(m.ID, id) && (templateID == "" || NewerModel(id, templateID)) {
					template, templateID = member, id
				}
			}
			if templateID == "" {
				continue
			}
			if template.Effort != "" && !slices.Contains(m.Efforts, template.Effort) {
				notes = append(notes, fmt.Sprintf("%s: %s не добавлена в пул — effort %s не подтверждён каталогом.", name, key, template.Effort))
				continue
			}
			l.ModelPools[name] = append(l.ModelPools[name], PoolTarget{Model: key, Effort: template.Effort})
		}
		for profileName, profile := range original.Profiles {
			if profileName == original.ActiveProfile {
				continue
			}
			current := l.Profiles[profileName]
			for poolName, pool := range profile.ModelPools {
				present := false
				for _, member := range current.ModelPools[poolName] {
					if member.Model == key {
						present = true
						break
					}
				}
				if present {
					continue
				}
				var template PoolTarget
				var templateID string
				for _, member := range pool {
					prefix := providerName + "/"
					if !strings.HasPrefix(member.Model, prefix) {
						continue
					}
					id := strings.TrimPrefix(member.Model, prefix)
					if CodexFamily(id) == family && NewerModel(m.ID, id) && (templateID == "" || NewerModel(id, templateID)) {
						template, templateID = member, id
					}
				}
				if templateID == "" {
					continue
				}
				if template.Effort != "" && !slices.Contains(m.Efforts, template.Effort) {
					notes = append(notes, fmt.Sprintf("%s/%s: %s не добавлена в пул — effort %s не подтверждён каталогом.", profileName, poolName, key, template.Effort))
					continue
				}
				current.ModelPools[poolName] = append(current.ModelPools[poolName], PoolTarget{Model: key, Effort: template.Effort})
			}
			l.Profiles[profileName] = current
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	l.Catalog.CodexSeen[providerName] = ids
	return notes
}
