package privacy

import (
	"errors"
	"fmt"
	"regexp"
)

// PatternRule detects an organisation-specific entity. Group selects the
// sensitive capture, leaving labels and surrounding command syntax untouched.
type PatternRule struct {
	Name  string `json:"name"`
	Kind  Kind   `json:"kind"`
	Regex string `json:"regex"`
	Group int    `json:"group"`
}
type patternDetector struct {
	rule PatternRule
	re   *regexp.Regexp
}

func compilePattern(rule PatternRule) (patternDetector, error) {
	if rule.Name == "" || (!mappedKind(rule.Kind) && rule.Kind != KindSecret) {
		return patternDetector{}, errors.New("patterns: name and supported kind required")
	}
	re, err := regexp.Compile(rule.Regex)
	if err != nil {
		return patternDetector{}, fmt.Errorf("patterns.%s: %w", rule.Name, err)
	}
	if re.MatchString("") || rule.Group < 0 || rule.Group > re.NumSubexp() {
		return patternDetector{}, fmt.Errorf("patterns.%s: invalid capture or empty match", rule.Name)
	}
	return patternDetector{rule, re}, nil
}
func (d patternDetector) Detect(text string) []Span {
	var out []Span
	for _, v := range d.re.FindAllStringSubmatchIndex(text, -1) {
		a, b := v[2*d.rule.Group], v[2*d.rule.Group+1]
		if a >= 0 && b > a {
			out = append(out, Span{a, b, d.rule.Kind, text[a:b]})
		}
	}
	return out
}
