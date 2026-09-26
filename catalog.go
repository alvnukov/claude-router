package main

import (
	"context"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	conf "localrouter/internal/config"
)

const catalogRefreshInterval = time.Hour
const anthropicCatalogURL = "https://platform.claude.com/docs/en/models/overview"

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
	snapshot := u.cs.Get().Local
	ids, fetchErr := u.fetchAnthropic(ctx)
	results := map[string]conf.ProviderProbe{}
	for _, p := range snapshot.Providers {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res := u.probeProvider(p, true)
		probe := conf.ProviderProbe{OK: res.OK, Msg: res.Msg, At: res.At}
		for _, m := range res.Info {
			probe.Models = append(probe.Models, catalogModel{ID: m.ID, Name: m.Name, Efforts: append([]string(nil), m.Efforts...)})
		}
		results[p.Name] = probe
	}
	return u.cs.UpdateCatalog(snapshot, ids, fetchErr, results, u.life)
}

func (u *uiServer) settingsRefreshModels(w http.ResponseWriter, r *http.Request) {
	if !sameOriginPost(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	err := u.refreshModels(r.Context())
	u.renderSettingsResult(w, err, "Проверка моделей завершена")
}
