package privacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strings"
)

// Entry is one dictionary entry of privacy.json: the forms of a name and,
// if set, the pseudonym it always gets. A form ending in * is a stem.
type Entry struct {
	Kind      Kind
	Forms     []string
	Pseudonym string
}

// Rules are the owner's rules from privacy.json, validated and with every
// declared network assigned its block in the pseudonym range.
type Rules struct {
	Filters      map[string]bool // absent means enabled; immutable after parsing
	Fields       []FieldRule
	fields       []fieldRule
	Patterns     []PatternRule
	patterns     []patternDetector
	Networks     []netip.Prefix // public networks of the organisation
	Domains      []string       // internal domain suffixes, lower case
	Entries      []Entry
	Allow        []string // values that are never masked
	AllowPaths   []string // JSON paths that are never masked
	PublicRange  netip.Prefix
	PublicRange6 netip.Prefix
	Sources      string // "withhold" or "pass"
	RetainDays   int

	policy *detectionPolicy
	blocks []netBlock // one per network, in declaration order
}

// netBlock pairs a declared network with its block of the same size in the
// pseudonym range.
type netBlock struct{ Real, Pseudo netip.Prefix }

var (
	defaultPublicRange  = netip.MustParsePrefix("100.64.0.0/10")
	defaultPublicRange6 = netip.MustParsePrefix("3fff::/20")

	// maskedClasses are masked whether declared or not; a declared network
	// must not overlap them, nor may a pseudonym range.
	maskedClasses = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("fd00::/8"),
		netip.MustParsePrefix("fe80::/10"),
	}

	allowPathRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\[(\*|[0-9]+)\])*(\.[A-Za-z_][A-Za-z0-9_]*(\[(\*|[0-9]+)\])*)*$`)
)

type rawRules struct {
	Filters      map[string]bool `json:"filters,omitempty"`
	Fields       []FieldRule     `json:"fields"`
	Patterns     []PatternRule   `json:"patterns"`
	Detection    json.RawMessage `json:"detection"`
	Networks     []string        `json:"networks"`
	Domains      []string        `json:"domains"`
	Entries      []rawEntry      `json:"entries"`
	Allow        []string        `json:"allow"`
	AllowPaths   []string        `json:"allow_paths"`
	PublicRange  string          `json:"public_range"`
	PublicRange6 string          `json:"public_range6"`
	Sources      string          `json:"sources"`
	RetainDays   *int            `json:"retain_days"`
}

type rawEntry struct {
	Kind      string   `json:"kind"`
	Forms     []string `json:"forms"`
	Pseudonym string   `json:"pseudonym"`
}

// LoadRules reads and validates privacy.json.
func LoadRules(path string) (*Rules, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("privacy.json: %w", err)
	}
	return ParseRules(data)
}

// ParseRules validates privacy.json. Any rule it cannot honour exactly is an
// error: masking with a rule half applied would leak.
func ParseRules(data []byte) (*Rules, error) {
	r, err := parseRules(data)
	if err != nil {
		return nil, fmt.Errorf("privacy.json: %w", err)
	}
	return r, nil
}

func parseRules(data []byte) (*Rules, error) {
	root, err := scanJSON(data)
	if err != nil {
		return nil, err
	}
	if root.kind != '{' {
		return nil, errors.New("expected rules object")
	}
	if filters := root.get("filters"); filters != nil {
		if filters.kind != '{' {
			return nil, errors.New("filters: expected object")
		}
		for _, p := range filters.pairs {
			if p.value.kind != 't' && p.value.kind != 'f' {
				return nil, errors.New("filters: expected boolean")
			}
		}
	}
	var raw rawRules
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the object")
	}
	r := &Rules{
		Filters:      raw.Filters,
		PublicRange:  defaultPublicRange,
		PublicRange6: defaultPublicRange6,
		Sources:      "withhold",
		RetainDays:   30,
		Allow:        raw.Allow,
	}
	for name := range r.Filters {
		if !slices.Contains(FilterIDs(), name) {
			return nil, errors.New("unknown filter")
		}
	}
	r.policy, err = compileDetection(raw.Detection)
	if err != nil {
		return nil, err
	}
	for _, rule := range raw.Fields {
		f, err := compileField(rule)
		if err != nil {
			return nil, err
		}
		r.Fields = append(r.Fields, rule)
		r.fields = append(r.fields, f)
	}
	for _, rule := range raw.Patterns {
		p, err := compilePattern(rule)
		if err != nil {
			return nil, err
		}
		r.Patterns = append(r.Patterns, rule)
		r.patterns = append(r.patterns, p)
	}
	if raw.PublicRange != "" {
		if r.PublicRange, err = parseRange("public_range", raw.PublicRange, true); err != nil {
			return nil, err
		}
	}
	if raw.PublicRange6 != "" {
		if r.PublicRange6, err = parseRange("public_range6", raw.PublicRange6, false); err != nil {
			return nil, err
		}
	}
	if err := r.parseNetworks(raw.Networks); err != nil {
		return nil, err
	}
	for i, d := range raw.Domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" {
			return nil, fmt.Errorf("domains[%d]: empty", i)
		}
		r.Domains = append(r.Domains, d)
	}
	for i, e := range raw.Entries {
		entry, err := parseEntry(e)
		if err != nil {
			return nil, fmt.Errorf("entries[%d]: %w", i, err)
		}
		r.Entries = append(r.Entries, entry)
	}
	for i, v := range raw.Allow {
		if v == "" {
			return nil, fmt.Errorf("allow[%d]: empty", i)
		}
	}
	for i, p := range raw.AllowPaths {
		if !allowPathRE.MatchString(p) {
			return nil, fmt.Errorf("allow_paths[%d]: %q is not a path like messages[*].content", i, p)
		}
	}
	r.AllowPaths = raw.AllowPaths
	switch raw.Sources {
	case "", "withhold":
	case "pass":
		r.Sources = "pass"
	default:
		return nil, fmt.Errorf("sources: %q is neither withhold nor pass", raw.Sources)
	}
	if raw.RetainDays != nil {
		if *raw.RetainDays < 1 {
			return nil, fmt.Errorf("retain_days: %d, want at least 1", *raw.RetainDays)
		}
		r.RetainDays = *raw.RetainDays
	}
	return r, nil
}

// parseRange reads a pseudonym range: the right family, clear of the masked
// classes.
func parseRange(field, s string, v4 bool) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%s: %w", field, err)
	}
	p = p.Masked()
	if p.Addr().Is4() != v4 {
		return netip.Prefix{}, fmt.Errorf("%s: %s is the wrong address family", field, p)
	}
	for _, c := range maskedClasses {
		if p.Overlaps(c) {
			return netip.Prefix{}, fmt.Errorf("%s: %s overlaps the masked class %s", field, p, c)
		}
	}
	return p, nil
}

// parseNetworks validates the declared networks and gives each one a block
// of its own size in the pseudonym range of its family: in declaration
// order, each block aligned to its size.
func (r *Rules) parseNetworks(nets []string) error {
	cursor := map[bool]*big.Int{true: new(big.Int), false: new(big.Int)}
	for i, s := range nets {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return fmt.Errorf("networks[%d]: %w", i, err)
		}
		p = p.Masked()
		for _, c := range maskedClasses {
			if p.Overlaps(c) {
				return fmt.Errorf("networks[%d]: %s overlaps the masked class %s", i, p, c)
			}
		}
		for _, q := range r.Networks {
			if p.Overlaps(q) {
				return fmt.Errorf("networks[%d]: %s overlaps %s", i, p, q)
			}
		}
		field, rng := "public_range", r.PublicRange
		if !p.Addr().Is4() {
			field, rng = "public_range6", r.PublicRange6
		}
		if p.Overlaps(rng) {
			return fmt.Errorf("networks[%d]: %s overlaps the pseudonym range %s (%s)", i, p, rng, field)
		}
		pseudo, err := place(cursor[p.Addr().Is4()], p, rng)
		if err != nil {
			return fmt.Errorf("networks[%d]: %s does not fit in %s %s: %w", i, p, field, rng, err)
		}
		r.Networks = append(r.Networks, p)
		r.blocks = append(r.blocks, netBlock{Real: p, Pseudo: pseudo})
	}
	return nil
}

// place aligns cursor, an offset into rng, to the size of p, takes a block
// of that size there and moves cursor past it.
func place(cursor *big.Int, p, rng netip.Prefix) (netip.Prefix, error) {
	if p.Bits() < rng.Bits() {
		return netip.Prefix{}, errors.New("the network is larger than the range")
	}
	bits := p.Addr().BitLen()
	size := new(big.Int).Lsh(big.NewInt(1), uint(bits-p.Bits()))
	limit := new(big.Int).Lsh(big.NewInt(1), uint(bits-rng.Bits()))
	// cursor = ceil(cursor / size) * size
	cursor.Add(cursor, size).Sub(cursor, big.NewInt(1))
	cursor.Div(cursor, size).Mul(cursor, size)
	end := new(big.Int).Add(cursor, size)
	if end.Cmp(limit) > 0 {
		return netip.Prefix{}, errors.New("the range is full")
	}
	start := new(big.Int).SetBytes(rng.Addr().AsSlice())
	start.Add(start, cursor)
	cursor.Set(end)
	buf := start.FillBytes(make([]byte, bits/8))
	addr, _ := netip.AddrFromSlice(buf)
	return netip.PrefixFrom(addr, p.Bits()), nil
}

func parseEntry(e rawEntry) (Entry, error) {
	kind := Kind(e.Kind)
	if !slices.Contains(entryKinds, kind) {
		return Entry{}, fmt.Errorf("unknown kind %q", e.Kind)
	}
	if len(e.Forms) == 0 {
		return Entry{}, errors.New("no forms")
	}
	for _, f := range e.Forms {
		if strings.TrimSpace(f) == "" {
			return Entry{}, errors.New("empty form")
		}
		stem, isStem := strings.CutSuffix(f, "*")
		if strings.Contains(stem, "*") || (isStem && strings.TrimSpace(stem) == "") {
			return Entry{}, fmt.Errorf("form %q: * only ends a stem", f)
		}
	}
	return Entry{Kind: kind, Forms: e.Forms, Pseudonym: e.Pseudonym}, nil
}
