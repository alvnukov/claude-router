package main

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
)

// Legacy fields are consumed once; their known routes become explicit entries.
func MigrateLegacyPools(l localSetup, cloudOnly []string) (localSetup, bool) {
	if l.Routes != nil {
		return l, false
	}
	l = l.Clone()
	l.Routes = map[string]map[string]modelRoute{}
	l.ModelPools = map[string][]poolTarget{}
	var keys []string
	for _, m := range l.Ordered() {
		keys = append(keys, m.Key())
	}
	// Freeze the old catalog: adding a new dashboard model must not enable it
	// through a legacy rule unless that ID was explicitly configured already.
	legacyModels := []string{"claude-opus-5", "claude-fable-5", "claude-sonnet-5", "claude-haiku-4-5"}
	models := append(legacyModels, cloudOnly...)
	localIDs := map[string]bool{}
	for _, configured := range l.Models {
		localIDs[configured.Model] = true
		models = append(models, configured.Model)
	}
	for model := range l.Pools {
		models = append(models, model)
	}
	if len(keys) > 0 {
		models = append(models, "local-model")
	}
	sort.Strings(models)
	for _, model := range models {
		if _, exists := l.Routes[model]; exists {
			continue
		}
		targets := keys
		direct := len(cloudOnly) == 0 && model != "local-model"
		for _, pattern := range cloudOnly {
			if strings.Contains(model, pattern) {
				direct = true
			}
		}
		best := ""
		for pattern := range l.Pools {
			if strings.Contains(strings.ToLower(model), strings.ToLower(pattern)) && (len(pattern) > len(best) || len(pattern) == len(best) && pattern < best) {
				best = pattern
			}
		}
		if best != "" {
			targets = l.Pools[best]
			direct = len(targets) == 0
		}
		// Configured model IDs took precedence over cloud-only patterns in
		// the old router, even when an ID also matched a Claude model.
		if localIDs[model] {
			direct = false
			if len(targets) == 0 {
				targets = keys
			}
		}
		l.Routes[model] = map[string]modelRoute{}
		for _, effort := range ClaudeEfforts {
			route := modelRoute{Mode: "disabled"}
			if direct {
				route.Mode = "anthropic"
			} else if len(targets) > 0 {
				members := legacyTargets(l, targets, effort)
				name := "Перенесённый " + model + " / " + effort
				// Reuse identical pools instead of multiplying six copies.
				names := make([]string, 0, len(l.ModelPools))
				for n := range l.ModelPools {
					names = append(names, n)
				}
				sort.Strings(names)
				for _, n := range names {
					if reflect.DeepEqual(l.ModelPools[n], members) {
						name = n
						break
					}
				}
				l.ModelPools[name] = members
				route = modelRoute{Mode: "pool", Pool: name}
			}
			l.Routes[model][effort] = route
		}
	}
	l.Preferred, l.Pools = "", nil
	for i := range l.Models {
		l.Models[i].Efforts = nil
	}
	return l, true
}

func SavePoolMigration(path string, l localSetup) error {
	return SaveConfigurationMigration(path, l, ".before-pools")
}

func SaveConfigurationMigration(path string, l localSetup, suffix string) error {
	if path == "" {
		return nil
	}
	if data, err := os.ReadFile(path); err == nil {
		backup, err := os.OpenFile(path+suffix, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, err = backup.Write(data)
			closeErr := backup.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		} else if !os.IsExist(err) {
			return fmt.Errorf("backup: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return WriteProviders(path, l)
}

func legacyTargets(l localSetup, keys []string, source string) []poolTarget {
	targets := make([]poolTarget, 0, len(keys))
	for _, key := range keys {
		target := poolTarget{Model: key}
		for _, model := range l.Models {
			if model.Key() == key {
				target.Effort = model.Efforts[source]
				break
			}
		}
		targets = append(targets, target)
	}
	return targets
}
