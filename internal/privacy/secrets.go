package privacy

import (
	"strings"
)

func (p *detectionPolicy) secretKeyword(k string) bool {
	k = strings.ToLower(k)
	for _, s := range p.rules.SecretKeywords {
		if k == s || strings.HasSuffix(k, "_"+s) {
			return true
		}
	}
	return false
}
func placeholderValue(s string) bool {
	return strings.HasPrefix(s, "${") || strings.HasPrefix(s, "{{") || strings.HasPrefix(s, "<") || strings.Trim(s, "*") == ""
}
func detectSecrets(text string) []Span { return defaultPolicy.detectSecrets(text) }
func (p *detectionPolicy) detectSecrets(text string) []Span {
	var out []Span
	add := func(start, end int) {
		if start >= 0 && end > start && !placeholderValue(text[start:end]) {
			out = append(out, Span{start, end, KindSecret, text[start:end]})
		}
	}
	for _, v := range p.prefix.FindAllStringIndex(text, -1) {
		add(v[0], v[1])
	}
	for _, v := range p.assignment.FindAllStringSubmatchIndex(text, -1) {
		if !p.secretKeyword(text[v[2]:v[3]]) {
			continue
		}
		for i := 4; i < len(v); i += 2 {
			if v[i] >= 0 && v[i+1] > v[i] {
				add(v[i], v[i+1])
				break
			}
		}
	}
	for _, re := range p.contexts {
		for _, v := range re.FindAllStringSubmatchIndex(text, -1) {
			add(v[2], v[3])
		}
	}
	return out
}
func secretFamily(s string) string {
	for _, p := range []struct{ prefix, name string }{{"sk-ant-", "anthropic"}, {"sk-", "openai"}, {"AKIA", "aws"}, {"ghp_", "github"}, {"xox", "slack"}, {"eyJ", "jwt"}, {"-----BEGIN", "private-key"}} {
		if strings.HasPrefix(s, p.prefix) {
			return p.name
		}
	}
	return "credential"
}
