package main

import (
	"context"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

const catalogRefreshInterval = time.Hour
const anthropicCatalogURL = "https://platform.claude.com/docs/en/models/overview"

type catalogModel struct {
	ID      string   `json:"id"`
	Name    string   `json:"name,omitempty"`
	Efforts []string `json:"efforts,omitempty"`
}
type providerCatalog struct {
	Models    []catalogModel `json:"models"`
	UpdatedAt time.Time      `json:"updated_at"`
	Error     string         `json:"error,omitempty"`
}
type modelCatalog struct {
	Anthropic        []string                   `json:"anthropic,omitempty"`
	AnthropicUpdated time.Time                  `json:"anthropic_updated,omitempty"`
	Providers        map[string]providerCatalog `json:"providers,omitempty"`
	CodexSeen        map[string][]string        `json:"codex_seen,omitempty"`
	CheckedAt        time.Time                  `json:"checked_at,omitempty"`
	Notes            []string                   `json:"notes,omitempty"`
}

func (c modelCatalog) clone() modelCatalog {
	c.Anthropic = append([]string(nil), c.Anthropic...)
	c.Notes = append([]string(nil), c.Notes...)
	providers := map[string]providerCatalog{}
	for key, value := range c.Providers {
		value.Models = append([]catalogModel(nil), value.Models...)
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

var catalogButtonPattern = regexp.MustCompile(`(?s)<button\b[^>]*>(.*?)</button>`)
var catalogTagPattern = regexp.MustCompile(`<[^>]*>`)
var catalogIDPattern = regexp.MustCompile(`^claude-[a-z]+-[0-9]+(?:-[0-9]+)*$`)

// Only copyable model IDs from the official comparison table are catalog
// entries. Navigation links and migration examples are not API identifiers.
func parseAnthropicCatalog(body []byte) ([]string, error) {
	ids := map[string]bool{}
	for _, match := range catalogButtonPattern.FindAllSubmatch(body, -1) {
		id := strings.TrimSpace(html.UnescapeString(catalogTagPattern.ReplaceAllString(string(match[1]), "")))
		if catalogIDPattern.MatchString(id) {
			ids[id] = true
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("официальный каталог не содержит распознанных ID моделей")
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func fetchAnthropicCatalog(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", anthropicCatalogURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "claude-router/1.0")
	req.Header.Set("Accept", "text/html")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 2<<20 {
		return nil, fmt.Errorf("каталог слишком велик")
	}
	return parseAnthropicCatalog(body)
}

func (u *uiServer) startCatalogUpdates(ctx context.Context) {
	go func() {
		// Populate immediately on startup, then check once per hour.
		if err := u.refreshModels(ctx); err != nil {
			log.Printf("model catalog refresh: %v", err)
		}
		ticker := time.NewTicker(catalogRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := u.refreshModels(ctx); err != nil {
					log.Printf("model catalog refresh: %v", err)
				}
			}
		}
	}()
}

// Network reads happen outside the config lock. Results are merged into the
// latest configuration, so an hourly update never overwrites a dashboard edit.
func (u *uiServer) refreshModels(ctx context.Context) error {
	if u.life != nil && !u.life.writesSharedState() {
		return fmt.Errorf("model catalog refresh requires active instance")
	}
	u.catalogMu.Lock()
	defer u.catalogMu.Unlock()
	snapshot := u.cs.get().local
	ids, fetchErr := u.fetchAnthropic(ctx)
	results := map[string]probeResult{}
	for _, p := range snapshot.Providers {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		results[p.Name] = u.probeProvider(p, true)
	}
	u.cs.mu.Lock()
	defer u.cs.mu.Unlock()
	if u.life != nil && !u.life.writesSharedState() {
		return fmt.Errorf("model catalog refresh requires active instance")
	}
	next := u.cs.c.local.clone()
	next.Catalog.CheckedAt = time.Now()
	next.Catalog.Notes = nil
	if fetchErr != nil {
		next.Catalog.Notes = append(next.Catalog.Notes, "Anthropic: "+fetchErr.Error()+". Сохранён предыдущий каталог.")
	} else {
		next.Catalog.Anthropic = ids
		next.Catalog.AnthropicUpdated = time.Now()
	}
	for _, p := range next.Providers {
		res, exists := results[p.Name]
		before, wasPresent := snapshot.provider(p.Name)
		if !exists || !wasPresent || before != p {
			continue
		}
		cached := next.Catalog.Providers[p.Name]
		if !res.OK {
			cached.Error = res.Msg
			next.Catalog.Notes = append(next.Catalog.Notes, p.Name+": "+res.Msg+". Сохранён предыдущий каталог.")
		} else {
			cached = providerCatalog{UpdatedAt: res.At}
			for _, m := range res.Info {
				cached.Models = append(cached.Models, catalogModel{ID: m.ID, Name: m.Name, Efforts: append([]string(nil), m.Efforts...)})
			}
			if p.Type == "codex" {
				next.Catalog.Notes = append(next.Catalog.Notes, inheritCodexModels(&next, p.Name, cached.Models)...)
			}
		}
		next.Catalog.Providers[p.Name] = cached
	}
	next.repairInactiveProfiles()
	if err := next.validateProfiles(); err != nil {
		return err
	}
	if u.cs.provPath != "" {
		if err := writeProviders(u.cs.provPath, next); err != nil {
			return err
		}
		u.cs.wroteProviders()
	}
	u.cs.c.local = next
	log.Printf("model catalogs refreshed: anthropic=%d providers=%d notes=%d", len(next.Catalog.Anthropic), len(results), len(next.Catalog.Notes))
	return nil
}

// New Codex versions append to each matching pool using that pool's newest
// earlier member as the effort template. A removed model is not re-added on
// the next refresh, and distinct named variants never inherit from each other.
func inheritCodexModels(l *localSetup, providerName string, models []catalogModel) []string {
	if l.Catalog.CodexSeen == nil {
		l.Catalog.CodexSeen = map[string][]string{}
	}
	seen := map[string]bool{}
	for _, id := range l.Catalog.CodexSeen[providerName] {
		seen[id] = true
	}
	original := l.clone()
	var notes []string
	for _, m := range models {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		key := providerName + "/" + m.ID
		if !l.hasModel(key) {
			l.Models = append(l.Models, localModel{Provider: providerName, Model: m.ID})
		}
		family := codexFamily(m.ID)
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
			var template poolTarget
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
				if codexFamily(id) == family && newerModel(m.ID, id) && (templateID == "" || newerModel(id, templateID)) {
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
			l.ModelPools[name] = append(l.ModelPools[name], poolTarget{Model: key, Effort: template.Effort})
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
				var template poolTarget
				var templateID string
				for _, member := range pool {
					prefix := providerName + "/"
					if !strings.HasPrefix(member.Model, prefix) {
						continue
					}
					id := strings.TrimPrefix(member.Model, prefix)
					if codexFamily(id) == family && newerModel(m.ID, id) && (templateID == "" || newerModel(id, templateID)) {
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
				current.ModelPools[poolName] = append(current.ModelPools[poolName], poolTarget{Model: key, Effort: template.Effort})
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

func (u *uiServer) settingsRefreshModels(w http.ResponseWriter, r *http.Request) {
	if !sameOriginPost(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	err := u.refreshModels(r.Context())
	u.renderSettingsResult(w, err, "Проверка моделей завершена")
}
