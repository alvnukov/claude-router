package privacy

import (
	"fmt"
	"regexp"
	"strings"
)

// FieldRule supplies explicit semantic context when identical text can mean
// different things, for example an account name and a server name.
type FieldRule struct {
	Path string `json:"path"`
	Kind Kind   `json:"kind"`
}
type fieldRule struct {
	rule FieldRule
	path *regexp.Regexp
}

func pathPattern(path string) *regexp.Regexp {
	pattern := regexp.QuoteMeta(path)
	pattern = strings.ReplaceAll(pattern, `\[\*\]`, `\[[0-9]+\]`)
	return regexp.MustCompile("^" + pattern + "$")
}
func compileField(rule FieldRule) (fieldRule, error) {
	if !allowPathRE.MatchString(rule.Path) || (!mappedKind(rule.Kind) && rule.Kind != KindSecret) {
		return fieldRule{}, fmt.Errorf("fields: invalid path or kind")
	}
	return fieldRule{rule, pathPattern(fieldPath(rule.Path))}, nil
}
func (e *Engine) fieldKind(f fieldKind) Kind {
	if f.objectKey {
		return ""
	}
	for _, r := range e.rules.fields {
		if r.path.MatchString(fieldPath(f.path)) {
			return r.rule.Kind
		}
	}
	if enabled(e.rules.Filters, "secret") && f.property != "" && e.rules.policy.secretKeyword(f.property) {
		return KindSecret
	}
	return ""
}

// History, a JSON response and streaming content share the same field roles.
func fieldPath(path string) string {
	if strings.HasPrefix(path, "messages[") {
		if end := strings.Index(path, "]."); end >= 0 {
			return path[end+2:]
		}
	}
	return path
}
