package privacy

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// Default detection rules are data, also usable as a starting configuration.
// A privacy.json detection object may override individual fields. Compile once
// on load, never in the request path. Loaded engines are immutable snapshots.
//
//go:embed defaults.json
var defaultDetection []byte

type detectionRules struct {
	IPv4           string   `json:"ipv4"`
	IPv6           string   `json:"ipv6"`
	MAC            string   `json:"mac"`
	Host           string   `json:"host"`
	Email          string   `json:"email"`
	Phone          string   `json:"phone"`
	SecretPrefix   string   `json:"secret_prefix"`
	Assignment     string   `json:"assignment"`
	SecretContexts []string `json:"secret_contexts"`
	SecretKeywords []string `json:"secret_keywords"`
	Endings        []string `json:"endings"`
}
type detectionPolicy struct {
	rules                                                   detectionRules
	ipv4, ipv6, mac, host, email, phone, prefix, assignment *regexp.Regexp
	contexts                                                []*regexp.Regexp
}

func compileDetection(overrides []byte) (*detectionPolicy, error) {
	var r detectionRules
	if err := json.Unmarshal(defaultDetection, &r); err != nil {
		return nil, err
	}
	if len(overrides) > 0 {
		dec := json.NewDecoder(bytes.NewReader(overrides))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			return nil, fmt.Errorf("detection: %w", err)
		}
	}
	p := &detectionPolicy{rules: r}
	for _, field := range []struct {
		name, expr string
		out        **regexp.Regexp
	}{{"ipv4", r.IPv4, &p.ipv4}, {"ipv6", r.IPv6, &p.ipv6}, {"mac", r.MAC, &p.mac}, {"host", r.Host, &p.host}, {"email", r.Email, &p.email}, {"phone", r.Phone, &p.phone}, {"secret_prefix", r.SecretPrefix, &p.prefix}, {"assignment", r.Assignment, &p.assignment}} {
		if field.expr == "" {
			return nil, fmt.Errorf("detection.%s: empty pattern", field.name)
		}
		re, err := regexp.Compile(field.expr)
		if err != nil {
			return nil, fmt.Errorf("detection.%s: %w", field.name, err)
		}
		*field.out = re
	}
	if p.assignment.NumSubexp() != 4 {
		return nil, errors.New("detection.assignment: need key and three value capture groups")
	}
	for i, expr := range r.SecretContexts {
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("detection.secret_contexts[%d]: %w", i, err)
		}
		if re.NumSubexp() != 1 {
			return nil, errors.New("detection.secret_contexts: need one value capture group")
		}
		p.contexts = append(p.contexts, re)
	}
	return p, nil
}

var defaultPolicy = func() *detectionPolicy {
	p, err := compileDetection(nil)
	if err != nil {
		panic(err)
	}
	return p
}()
