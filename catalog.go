package main

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"localrouter/internal/catalogstartup"
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

func fetchAnthropicCatalog(ctx context.Context, deps ...catalogstartup.Dependencies) ([]string, error) {
	var d catalogstartup.Dependencies
	if len(deps) > 0 {
		d = deps[0]
	}
	body, err := d.FetchOfficial(ctx, anthropicCatalogURL)
	if err != nil {
		return nil, err
	}
	return parseAnthropicCatalog(body)
}

func (u *uiServer) startCatalogUpdates(ctx context.Context) catalogstartup.Run {
	var deps catalogstartup.Dependencies
	if u.catalog != nil {
		deps = *u.catalog
	}
	return deps.Start(ctx, catalogRefreshInterval, u.refreshModels)
}

// Network reads happen outside the config lock. Results are merged into the
// latest configuration, so an hourly update never overwrites a dashboard edit.
func (u *uiServer) refreshModels(ctx context.Context) error {
	if err := catalogstartup.RequireActive(u.life == nil || u.life.writesSharedState()); err != nil {
		return err
	}
	u.catalogMu.Lock()
	defer u.catalogMu.Unlock()
	snapshot := u.cs.Get().Local
	ids, fetchErr := u.fetchAnthropic(ctx)
	probed, err := catalogstartup.Collect(ctx, snapshot.Providers, func(p provider) (string, probeResult) {
		return p.Name, u.probeProvider(p, true, ctx)
	})
	if err != nil {
		return err
	}
	results := map[string]conf.ProviderProbe{}
	for name, res := range probed {
		probe := conf.ProviderProbe{OK: res.OK, Msg: res.Msg, At: res.At}
		for _, m := range res.Info {
			probe.Models = append(probe.Models, catalogModel{ID: m.ID, Name: m.Name, Efforts: append([]string(nil), m.Efforts...)})
		}
		results[name] = probe
	}
	return u.cs.UpdateCatalog(ctx, snapshot, ids, fetchErr, results, u.life)
}

func (u *uiServer) settingsRefreshModels(w http.ResponseWriter, r *http.Request) {
	if !sameOriginPost(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	err := u.refreshModels(r.Context())
	u.renderSettingsResult(w, err, "Проверка моделей завершена", r.Context())
}
