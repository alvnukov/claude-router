package main

import (
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

var claudeFamilyPattern = regexp.MustCompile(`^(?:claude-)?([a-z]+)(?:-([0-9]+(?:-[0-9]+)*)(?:-latest)?)?$`)
var codexFamilyPattern = regexp.MustCompile(`^(gpt-)([0-9]+(?:\.[0-9]+)*)(.*)$`)

func ClaudeFamily(model string) string {
	match := claudeFamilyPattern.FindStringSubmatch(strings.ToLower(model))
	if match == nil {
		return ""
	}
	return match[1]
}

func modelVersion(model string) []int {
	var parts []string
	if match := codexFamilyPattern.FindStringSubmatch(model); match != nil {
		parts = strings.Split(match[2], ".")
	} else if match := claudeFamilyPattern.FindStringSubmatch(model); match != nil {
		parts = strings.Split(match[2], "-")
	}
	version := make([]int, 0, len(parts))
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err == nil {
			version = append(version, n)
		}
	}
	return version
}

func NewerModel(a, b string) bool {
	av, bv := modelVersion(a), modelVersion(b)
	for i := 0; i < max(len(av), len(bv)); i++ {
		x, y := 0, 0
		if i < len(av) {
			x = av[i]
		}
		if i < len(bv) {
			y = bv[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// Keep named variants separate: GPT Sol never inherits from Astra or Luna.
func CodexFamily(model string) string {
	match := codexFamilyPattern.FindStringSubmatch(model)
	if match == nil {
		return ""
	}
	return strings.TrimSuffix(match[1], "-") + match[3]
}

// The latest explicitly configured version supplies the initial family rules.
// Existing version overrides are preserved; subsequent family edits are live.
func MigrateFamilyRoutes(l localSetup) (localSetup, bool) {
	if l.FamilyRoutes != nil {
		return l, false
	}
	l = l.Clone()
	l.FamilyRoutes = map[string]map[string]modelRoute{}
	latest := map[string]string{}
	for model := range l.Routes {
		family := ClaudeFamily(model)
		if family == "" {
			continue
		}
		old, exists := latest[family]
		if !exists || NewerModel(model, old) || !NewerModel(old, model) && model < old {
			latest[family] = model
		}
	}
	for family, model := range latest {
		rules := map[string]modelRoute{}
		for effort, route := range l.Routes[model] {
			rules[effort] = route
		}
		l.FamilyRoutes[family] = rules
	}
	// Equal version rules now inherit dynamically. Keep genuinely different
	// version-specific rules as overrides.
	for model, rules := range l.Routes {
		if family := ClaudeFamily(model); family != "" && reflect.DeepEqual(rules, l.FamilyRoutes[family]) {
			delete(l.Routes, model)
		}
	}
	return l, true
}
