package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"localrouter/internal/platform"
)

// Local providers and models live in providers.json next to the binary (mode
// 0600, it holds API keys). The file is authoritative once it exists; before
// that the set is seeded from ROUTER_LOCAL_* in env, which stay only as that
// first-run seed. The UI writes the file; a hand edit is picked up within two
// seconds like env.

type provider struct {
	Name    string `json:"name"`
	Type    string `json:"type,omitempty"` // "" (OpenAI chat) or "codex" (ChatGPT subscription)
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key,omitempty"`
	AuthID  string `json:"auth_id,omitempty"` // Codex only: names this connection's credential file
}

type localModel struct {
	Provider string            `json:"provider"`
	Model    string            `json:"model"`
	Efforts  map[string]string `json:"efforts,omitempty"` // Claude effort -> provider effort
}

func (m localModel) Key() string { return m.Provider + "/" + m.Model }

type poolTarget struct {
	Model  string `json:"model"`
	Effort string `json:"effort,omitempty"`
}

type modelRoute struct {
	Mode   string `json:"mode"` // disabled, anthropic, pool, model
	Pool   string `json:"pool,omitempty"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

var anthropicModels = []string{"claude-opus-5-5", "claude-opus-5", "claude-fable-5-1", "claude-fable-5", "claude-sonnet-5", "claude-haiku-4-5", "claude-haiku-4-5-20251001"}

type localSetup struct {
	ActiveProfile string                           `json:"active_profile,omitempty"`
	Profiles      map[string]routingProfile        `json:"profiles,omitempty"`
	PoolSettings  map[string]poolSettings          `json:"pool_settings,omitempty"`
	FamilyRoutes  map[string]map[string]modelRoute `json:"family_routes"`
	Catalog       modelCatalog                     `json:"catalog,omitempty"`
	Providers     []provider                       `json:"providers"`
	Models        []localModel                     `json:"models"`
	Preferred     string                           `json:"preferred,omitempty"` // legacy input / request-scoped first pool member
	Pools         map[string][]string              `json:"pools,omitempty"`     // legacy, read only
	Routes        map[string]map[string]modelRoute `json:"routes"`
	ModelPools    map[string][]poolTarget          `json:"model_pools"`
}

func providersPath() string {
	if v, ok := os.LookupEnv("ROUTER_PROVIDERS_FILE"); ok {
		return v
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "providers.json")
	}
	return "providers.json"
}

func (l localSetup) provider(name string) (provider, bool) {
	for _, p := range l.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return provider{}, false
}

func (l localSetup) hasModel(key string) bool {
	for _, m := range l.Models {
		if m.Key() == key {
			return true
		}
	}
	return false
}

// ordered is the preferred model first, then the rest as configured.
func (l localSetup) ordered() []localModel {
	out := make([]localModel, 0, len(l.Models))
	for _, m := range l.Models {
		if m.Key() == l.Preferred {
			out = append(out, m)
		}
	}
	for _, m := range l.Models {
		if m.Key() != l.Preferred {
			out = append(out, m)
		}
	}
	return out
}

// ProvidersOf lists provider names, for templates.
func (l localSetup) ProviderNames() []string {
	out := make([]string, 0, len(l.Providers))
	for _, p := range l.Providers {
		out = append(out, p.Name)
	}
	return out
}

func providerNameOK(n string) bool {
	return n != "" && !strings.ContainsAny(n, "/, \t\n\"'")
}

// validate normalises in place and rejects anything the router could not act on.
func (l *localSetup) validate() error {
	seen := map[string]bool{}
	authIDs := map[string]bool{}
	for i := range l.Providers {
		p := &l.Providers[i]
		p.Name = strings.TrimSpace(p.Name)
		p.BaseURL = strings.TrimSuffix(strings.TrimSpace(p.BaseURL), "/")
		if p.Type != "" && p.Type != "codex" {
			return fmt.Errorf("provider %q: неизвестный тип %q", p.Name, p.Type)
		}
		if p.Type == "codex" {
			if p.BaseURL == "" {
				p.BaseURL = codexBaseURL
			}
			if p.BaseURL != codexBaseURL || p.APIKey != "" {
				return fmt.Errorf("provider %q: Codex использует фиксированный адрес и подписку, без API key", p.Name)
			}
			if p.AuthID == "" && p.Name != "codex" {
				return fmt.Errorf("provider %q: у подключения Codex нет auth_id; удалите его и добавьте заново через дашборд", p.Name)
			}
			if p.AuthID != "" && !authIDOK(p.AuthID) {
				return fmt.Errorf("provider %q: неверный auth_id", p.Name)
			}
			if len(p.Name) > 64 {
				return fmt.Errorf("provider %q: имя подключения Codex длиннее 64 байт", p.Name)
			}
			if p.AuthID != "" && authIDs[p.AuthID] {
				return fmt.Errorf("provider %q: auth_id повторяется", p.Name)
			}
			authIDs[p.AuthID] = true
		} else if p.AuthID != "" {
			return fmt.Errorf("provider %q: auth_id бывает только у Codex", p.Name)
		}
		if !providerNameOK(p.Name) {
			return fmt.Errorf("provider %q: имя без пробелов, «/», запятых и кавычек", p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("provider %q: имя повторяется", p.Name)
		}
		seen[p.Name] = true
		if u, err := url.Parse(p.BaseURL); err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("provider %q: нужен полный URL вида http://host:port/v1", p.Name)
		}
	}
	keys := map[string]bool{}
	var models []localModel
	for _, m := range l.Models {
		m.Provider = strings.TrimSpace(m.Provider)
		m.Model = strings.TrimSpace(m.Model)
		if !modelIDOK(m.Model) {
			return fmt.Errorf("model %q: без пробелов, запятых и кавычек", m.Model)
		}
		if !seen[m.Provider] {
			return fmt.Errorf("model %q: провайдер %q не существует", m.Model, m.Provider)
		}
		for source, target := range m.Efforts {
			if !validClaudeEffort(source) || !validProviderEffort(target) {
				return fmt.Errorf("model %q: неверное соответствие effort %q → %q", m.Key(), source, target)
			}
		}
		if keys[m.Key()] {
			continue
		}
		keys[m.Key()] = true
		models = append(models, m)
	}
	l.Models = models
	for pattern, pool := range l.Pools {
		if !modelIDOK(pattern) {
			return fmt.Errorf("pool %q: шаблон без пробелов, запятых и кавычек", pattern)
		}
		poolSeen := map[string]bool{}
		for _, key := range pool {
			if !keys[key] {
				return fmt.Errorf("pool %q: модель %q не настроена", pattern, key)
			}
			if poolSeen[key] {
				return fmt.Errorf("pool %q: модель %q повторяется", pattern, key)
			}
			poolSeen[key] = true
		}
	}

	for name, settings := range l.PoolSettings {
		if _, ok := l.ModelPools[name]; !ok {
			return fmt.Errorf("настройки несуществующего пула %q", name)
		}
		if err := settings.validate(); err != nil {
			return fmt.Errorf("пул %s: %w", name, err)
		}
	}
	for name, targets := range l.ModelPools {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("нужно название пула")
		}
		seen := map[string]bool{}
		for _, target := range targets {
			if !keys[target.Model] || seen[target.Model] {
				return fmt.Errorf("пул %s: модель %q отсутствует или повторяется", name, target.Model)
			}
			if target.Effort != "" && !validProviderEffort(target.Effort) {
				return fmt.Errorf("%s: неверный effort %q", target.Model, target.Effort)
			}
			seen[target.Model] = true
		}
	}
	for model, efforts := range l.allRouteRules() {
		if !modelIDOK(model) {
			return fmt.Errorf("неверный id модели %q", model)
		}
		for effort, route := range efforts {
			if !validClaudeEffort(effort) {
				return fmt.Errorf("%s: неизвестный effort %q", model, effort)
			}
			switch route.Mode {
			case "disabled", "anthropic":
				if route.Pool != "" || route.Model != "" || route.Effort != "" {
					return fmt.Errorf("%s / %s: адресат не соответствует маршруту", model, effort)
				}
			case "pool":
				if route.Model != "" || route.Effort != "" {
					return fmt.Errorf("%s / %s: модель допустима только для прямого маршрута", model, effort)
				}
				if _, ok := l.ModelPools[route.Pool]; !ok {
					return fmt.Errorf("пул %q не существует", route.Pool)
				}
			case "model":
				if route.Pool != "" || !keys[route.Model] {
					return fmt.Errorf("%s / %s: модель %q не настроена", model, effort, route.Model)
				}
				if route.Effort != "" && !validProviderEffort(route.Effort) {
					return fmt.Errorf("%s / %s: неверный effort %q", model, effort, route.Effort)
				}
			default:
				return fmt.Errorf("%s: неизвестный маршрут %q", model, route.Mode)
			}
		}
	}

	for family := range l.FamilyRoutes {
		if claudeFamily(family) != family {
			return fmt.Errorf("неверное семейство %q", family)
		}
	}
	return nil
}

func modelIDOK(m string) bool {
	return m != "" && !strings.ContainsAny(m, ", \t\n\"'")
}

func validClaudeEffort(value string) bool {
	switch value {
	case "default", "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

func validProviderEffort(value string) bool {
	switch value {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return true
	}
	return false
}

func readProviders(path string) (localSetup, error) {
	l, _, err := readProvidersWith(path, nil)
	return l, err
}

// readProvidersWith lets prepare change the setup before it is validated; the
// bool reports whether it did. Only startup passes a prepare, so a manual
// reload stays strict and never assigns anything.
func readProvidersWith(path string, prepare func(*localSetup) bool) (localSetup, bool, error) {
	l, err := readProvidersRaw(path)
	if err != nil {
		return l, false, err
	}
	changed := false
	if prepare != nil {
		changed = prepare(&l)
	}
	if err := l.validateProfiles(); err != nil {
		return l, changed, fmt.Errorf("%s: %w", path, err)
	}
	return l, changed, nil
}

func readProvidersRaw(path string) (localSetup, error) {
	var l localSetup
	data, err := os.ReadFile(path)
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(data, &l); err != nil {
		return l, fmt.Errorf("%s: %w", path, err)
	}
	pointer, err := os.ReadFile(path + ".active-profile")
	if err == nil {
		if l.Profiles != nil {
			return l, fmt.Errorf("%s: mixed inline and separate profiles", path)
		}
		if err := json.Unmarshal(pointer, &l.ActiveProfile); err != nil {
			return l, fmt.Errorf("active profile: %w", err)
		}
		entries, err := os.ReadDir(path + ".profiles")
		if err != nil {
			return l, err
		}
		l.Profiles = make(map[string]routingProfile, len(entries))
		for _, entry := range entries {
			name := strings.TrimSuffix(entry.Name(), ".json")
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || !profileNameOK(name) {
				return l, fmt.Errorf("invalid profile file %q", entry.Name())
			}
			body, err := os.ReadFile(filepath.Join(path+".profiles", entry.Name()))
			if err != nil {
				return l, err
			}
			var profile routingProfile
			if err := json.Unmarshal(body, &profile); err != nil {
				return l, fmt.Errorf("profile %q: %w", name, err)
			}
			l.Profiles[name] = profile
		}
	} else if !os.IsNotExist(err) {
		return l, err
	} else if _, dirErr := os.Stat(path + ".profiles"); dirErr == nil {
		return l, fmt.Errorf("%s: profile files exist without active pointer; migration interrupted", path)
	} else if !os.IsNotExist(dirErr) {
		return l, dirErr
	}
	if l.Profiles != nil {
		if err := l.useProfile(l.ActiveProfile); err != nil {
			return l, fmt.Errorf("%s: %w", path, err)
		}
	}
	return l, nil
}

func writeAtomicIfChanged(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil {
		if string(old) == string(data) {
			return nil
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return platform.ReplaceFile(tmp, path)
}

func writeActiveProfile(path, name string) error {
	data, err := json.Marshal(name)
	if err != nil {
		return err
	}
	return writeAtomicIfChanged(path+".active-profile", append(data, '\n'))
}

func writeProviders(path string, l localSetup) error {
	l = l.clone()
	active := l.ActiveProfile
	l.Preferred = "" // priority belongs to each pool, never the model catalog
	for i := range l.Models {
		l.Models[i].Efforts = nil
	}
	l.Pools = nil
	if l.Profiles != nil {
		if err := l.syncActiveProfile(); err != nil {
			return err
		}
		dir := path + ".profiles"
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		for name, profile := range l.Profiles {
			if !profileNameOK(name) {
				return fmt.Errorf("invalid profile name %q", name)
			}
			data, err := json.MarshalIndent(profile, "", "  ")
			if err != nil {
				return err
			}
			if err := writeAtomicIfChanged(filepath.Join(dir, name+".json"), append(data, '\n')); err != nil {
				return err
			}
		}
		l.FamilyRoutes, l.Routes, l.ModelPools, l.PoolSettings = nil, nil, nil, nil
		l.Profiles, l.ActiveProfile = nil, ""
	}
	if l.Profiles == nil && l.Routes == nil && l.ActiveProfile == "" {
		// Legacy configurations still need an explicit routes map for migration.
		if _, err := os.Stat(path + ".active-profile"); os.IsNotExist(err) {
			l.Routes = map[string]map[string]modelRoute{}
		}
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomicIfChanged(path, append(data, '\n')); err != nil {
		return err
	}
	if active != "" {
		return writeActiveProfile(path, active)
	}
	return nil
}

// seedFromEnv builds the initial setup from ROUTER_LOCAL_* for a router that
// has no providers.json yet. The provider is named after the host.
func seedFromEnv() localSetup {
	base := strings.TrimSuffix(env("ROUTER_LOCAL_BASE_URL", "http://127.0.0.1:1234/v1"), "/")
	name := "local"
	if u, err := url.Parse(base); err == nil && u.Hostname() != "" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" {
		name = strings.SplitN(u.Hostname(), ".", 2)[0]
	}
	l := localSetup{FamilyRoutes: map[string]map[string]modelRoute{}, Routes: map[string]map[string]modelRoute{}, ModelPools: map[string][]poolTarget{}, Providers: []provider{{Name: name, BaseURL: base, APIKey: os.Getenv("ROUTER_LOCAL_API_KEY")}}}
	first := strings.TrimSpace(env("ROUTER_LOCAL_MODEL", "local-model"))
	l.Models = append(l.Models, localModel{Provider: name, Model: first})
	for _, m := range strings.Split(os.Getenv("ROUTER_LOCAL_MODELS"), ",") {
		if m = strings.TrimSpace(m); m != "" {
			l.Models = append(l.Models, localModel{Provider: name, Model: m})
		}
	}
	l.Preferred = l.Models[0].Key()
	if err := l.validate(); err != nil {
		log.Printf("ROUTER_LOCAL_* seed: %v", err)
	}
	return l
}

// Only a missing file may be seeded from the environment. Existing-file errors
// must stop startup before any migration can write to disk. assigned reports
// that a Codex provider without auth_id got a fresh ID in memory; the caller
// persists it once (startup only). The seed path returns false.
func loadLocalSetupChecked(path string) (localSetup, bool, error) {
	if path != "" {
		l, assigned, err := readProvidersWith(path, assignCodexAuthIDs)
		if err == nil {
			return l, assigned, nil
		}
		if !os.IsNotExist(err) {
			return localSetup{}, false, err
		}
		if _, statErr := os.Stat(path); statErr == nil {
			return localSetup{}, false, err
		} else if !os.IsNotExist(statErr) {
			return localSetup{}, false, statErr
		}
	}
	return seedFromEnv(), false, nil
}

// clone copies the slices so an edit never touches the snapshot readers hold.
func (l localSetup) clone() localSetup {
	if l.Profiles != nil {
		profiles := make(map[string]routingProfile, len(l.Profiles))
		for name, p := range l.Profiles {
			c := localSetup{FamilyRoutes: p.FamilyRoutes, Routes: p.Routes, ModelPools: p.ModelPools, PoolSettings: p.PoolSettings}.clone()
			profiles[name] = routingProfile{c.FamilyRoutes, c.Routes, c.ModelPools, c.PoolSettings}
		}
		l.Profiles = profiles
	}
	if l.PoolSettings != nil {
		settings := make(map[string]poolSettings, len(l.PoolSettings))
		for name, value := range l.PoolSettings {
			settings[name] = value
		}
		l.PoolSettings = settings
	}
	l.Providers = append([]provider(nil), l.Providers...)
	l.Models = append([]localModel(nil), l.Models...)
	for i := range l.Models {
		if l.Models[i].Efforts != nil {
			copyMap := make(map[string]string, len(l.Models[i].Efforts))
			for k, v := range l.Models[i].Efforts {
				copyMap[k] = v
			}
			l.Models[i].Efforts = copyMap
		}
	}
	if l.Pools != nil {
		pools := make(map[string][]string, len(l.Pools))
		for k, v := range l.Pools {
			pools[k] = append([]string(nil), v...)
		}
		l.Pools = pools
	}
	if l.ModelPools != nil {
		pools := make(map[string][]poolTarget, len(l.ModelPools))
		for name, targets := range l.ModelPools {
			pools[name] = append([]poolTarget(nil), targets...)
		}
		l.ModelPools = pools
	}
	if l.Routes != nil {
		routes := make(map[string]map[string]modelRoute, len(l.Routes))
		for model, efforts := range l.Routes {
			copyEfforts := make(map[string]modelRoute, len(efforts))
			for effort, route := range efforts {
				copyEfforts[effort] = route
			}
			routes[model] = copyEfforts
		}
		l.Routes = routes
	}
	if l.FamilyRoutes != nil {
		families := map[string]map[string]modelRoute{}
		for family, rules := range l.FamilyRoutes {
			copyRules := map[string]modelRoute{}
			for effort, route := range rules {
				copyRules[effort] = route
			}
			families[family] = copyRules
		}
		l.FamilyRoutes = families
	}
	l.Catalog = l.Catalog.clone()

	return l
}

// Exact effort overrides win; otherwise the named model family supplies it.
func (l localSetup) routeFor(model, effort string) modelRoute {
	if effort == "" {
		effort = "default"
	}
	if route, ok := l.Routes[model][effort]; ok {
		return route
	}
	if route, ok := l.FamilyRoutes[claudeFamily(model)][effort]; ok {
		return route
	}
	return modelRoute{Mode: "disabled"}
}

// Flatten only for validation/reference checks; preserve independent override maps.
func (l localSetup) allRouteRules() map[string]map[string]modelRoute {
	out := map[string]map[string]modelRoute{}
	for key, rules := range l.Routes {
		out[key] = rules
	}
	for key, rules := range l.FamilyRoutes {
		out["family:"+key] = rules
	}
	return out
}
