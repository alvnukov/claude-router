package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Local providers and models live in providers.json next to the binary (mode
// 0600, it holds API keys). The file is authoritative once it exists; before
// that the set is seeded from ROUTER_LOCAL_* in env, which stay only as that
// first-run seed. The UI writes the file; a hand edit is picked up within two
// seconds like env.

type provider struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key,omitempty"`
}

type localModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

func (m localModel) Key() string { return m.Provider + "/" + m.Model }

type localSetup struct {
	Providers []provider   `json:"providers"`
	Models    []localModel `json:"models"`
	Preferred string       `json:"preferred"` // a model Key
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

func (l localSetup) preferredModel() (localModel, bool) {
	for _, m := range l.Models {
		if m.Key() == l.Preferred {
			return m, true
		}
	}
	return localModel{}, false
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
	for i := range l.Providers {
		p := &l.Providers[i]
		p.Name = strings.TrimSpace(p.Name)
		p.BaseURL = strings.TrimSuffix(strings.TrimSpace(p.BaseURL), "/")
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
		if keys[m.Key()] {
			continue
		}
		keys[m.Key()] = true
		models = append(models, m)
	}
	l.Models = models
	if l.Preferred == "" || !keys[l.Preferred] {
		l.Preferred = ""
		if len(models) > 0 {
			l.Preferred = models[0].Key()
		}
	}
	return nil
}

func modelIDOK(m string) bool {
	return m != "" && !strings.ContainsAny(m, ", \t\n\"'")
}

func readProviders(path string) (localSetup, error) {
	var l localSetup
	data, err := os.ReadFile(path)
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(data, &l); err != nil {
		return l, fmt.Errorf("%s: %w", path, err)
	}
	if err := l.validate(); err != nil {
		return l, fmt.Errorf("%s: %w", path, err)
	}
	return l, nil
}

func writeProviders(path string, l localSetup) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// seedFromEnv builds the initial setup from ROUTER_LOCAL_* for a router that
// has no providers.json yet. The provider is named after the host.
func seedFromEnv() localSetup {
	base := strings.TrimSuffix(env("ROUTER_LOCAL_BASE_URL", "http://127.0.0.1:1234/v1"), "/")
	name := "local"
	if u, err := url.Parse(base); err == nil && u.Hostname() != "" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" {
		name = strings.SplitN(u.Hostname(), ".", 2)[0]
	}
	l := localSetup{Providers: []provider{{Name: name, BaseURL: base, APIKey: os.Getenv("ROUTER_LOCAL_API_KEY")}}}
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

// loadLocalSetup prefers the file and falls back to env.
func loadLocalSetup(path string) localSetup {
	if path != "" {
		l, err := readProviders(path)
		switch {
		case err == nil:
			return l
		case !os.IsNotExist(err):
			log.Printf("providers: %v; using ROUTER_LOCAL_* from env", err)
		}
	}
	return seedFromEnv()
}

// clone copies the slices so an edit never touches the snapshot readers hold.
func (l localSetup) clone() localSetup {
	l.Providers = append([]provider(nil), l.Providers...)
	l.Models = append([]localModel(nil), l.Models...)
	return l
}
