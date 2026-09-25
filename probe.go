package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// A provider's GET /models carries whatever metadata that server keeps:
// capabilities, context size, availability, display names. The shape differs
// per server, so the router flattens each model object into dotted keys and
// derives the filters from the data itself rather than from a fixed schema.

type probeModel struct {
	ID      string
	Name    string            // a display name, when the provider has one
	Efforts []string          // Codex subscription levels reported for this model
	Desc    string            // a description, when the provider has one
	Attrs   map[string]string // flattened scalar fields, minus id/object
	Tags    []string          // the attrs worth showing inline, formatted
}

// facet is one filter: a key with few distinct values, or a numeric key
// filtered as "at least".
type facet struct {
	Key     string
	Short   string
	Numeric bool
	Values  []string // sorted distinct values
	Picked  string   // the selected value, "" for any
}

// parseModels reads an OpenAI-style model list with any extra fields.
func parseModels(body []byte) []probeModel {
	var ml struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(body, &ml) // not a model list: no models
	var out []probeModel
	for _, raw := range ml.Data {
		id, _ := raw["id"].(string)
		if id == "" {
			continue
		}
		m := probeModel{ID: id, Attrs: map[string]string{}}
		flatten("", raw, m.Attrs)
		delete(m.Attrs, "id")
		delete(m.Attrs, "object")
		for _, k := range []string{"displayName", "display_name", "name", "title"} {
			if v := m.Attrs[k]; v != "" && v != id {
				m.Name = v
				delete(m.Attrs, k)
				break
			}
		}
		for _, k := range []string{"description", "summary"} {
			if v := m.Attrs[k]; v != "" {
				m.Desc = v
				delete(m.Attrs, k)
				break
			}
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func flatten(prefix string, v any, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, y := range x {
			if prefix != "" {
				k = prefix + "." + k
			}
			flatten(k, y, out)
		}
	case []any:
		var ss []string
		for _, y := range x {
			if s, ok := y.(string); ok {
				ss = append(ss, s)
			}
		}
		if len(ss) == len(x) && len(ss) > 0 && len(ss) <= 8 {
			out[prefix] = strings.Join(ss, ",")
		}
	case bool:
		out[prefix] = strconv.FormatBool(x)
	case float64:
		out[prefix] = strconv.FormatFloat(x, 'f', -1, 64)
	case string:
		if x != "" {
			out[prefix] = x
		}
	}
}

// facetsOf picks the keys worth filtering on: booleans and short enums
// (2..8 distinct values) and numbers (any spread). Keys that are unique per
// model (hashes, timestamps) or long free text are left out; the search box
// covers those. It also fills each model's Tags from the same keys.
func facetsOf(models []probeModel) []facet {
	type agg struct {
		vals    map[string]int
		numeric bool
		n       int
	}
	keys := map[string]*agg{}
	for _, m := range models {
		for k, v := range m.Attrs {
			a := keys[k]
			if a == nil {
				a = &agg{vals: map[string]int{}, numeric: true}
				keys[k] = a
			}
			a.vals[v]++
			a.n++
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				a.numeric = false
			}
		}
	}
	var fs []facet
	for k, a := range keys {
		distinct := len(a.vals)
		long := false
		for v := range a.vals {
			if len(v) > 40 {
				long = true
			}
		}
		switch {
		case long:
			continue
		case a.numeric:
			if distinct < 2 {
				continue
			}
		case distinct < 2 || distinct > 8:
			continue
		case distinct == a.n && a.n > 2: // unique per model
			continue
		}
		f := facet{Key: k, Short: shortKey(k), Numeric: a.numeric}
		for v := range a.vals {
			f.Values = append(f.Values, v)
		}
		if f.Numeric {
			sort.Slice(f.Values, func(i, j int) bool { return numOf(f.Values[i]) < numOf(f.Values[j]) })
		} else {
			sort.Strings(f.Values)
		}
		fs = append(fs, f)
	}
	sort.Slice(fs, func(i, j int) bool {
		// numbers first (context size is the one people look for), then by key
		if fs[i].Numeric != fs[j].Numeric {
			return fs[i].Numeric
		}
		return fs[i].Key < fs[j].Key
	})
	// tags: numeric keys always, facet keys when set; bool false is noise
	var tagKeys []string
	for k, a := range keys {
		if a.numeric && len(a.vals) >= 1 {
			tagKeys = append(tagKeys, k)
		}
	}
	for _, f := range fs {
		if !f.Numeric {
			tagKeys = append(tagKeys, f.Key)
		}
	}
	sort.Strings(tagKeys)
	for i := range models {
		m := &models[i]
		seen := map[string]bool{}
		for _, k := range tagKeys {
			if seen[k] {
				continue
			}
			seen[k] = true
			v, ok := m.Attrs[k]
			if !ok || v == "false" {
				continue
			}
			m.Tags = append(m.Tags, fmtTag(k, v, keys[k].numeric))
		}
	}
	return fs
}

func shortKey(k string) string {
	if i := strings.LastIndex(k, "."); i >= 0 {
		return k[i+1:]
	}
	return k
}

func numOf(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func fmtNum(s string) string {
	f := numOf(s)
	switch {
	case f >= 1000000 && f == float64(int64(f)):
		return fmt.Sprintf("%.1fM", f/1000000)
	case f >= 10000 && f == float64(int64(f)):
		return fmt.Sprintf("%dk", int64(f/1000))
	}
	return s
}

func fmtTag(k, v string, numeric bool) string {
	switch {
	case numeric:
		return shortKey(k) + " " + fmtNum(v)
	case v == "true":
		return shortKey(k)
	}
	return shortKey(k) + ": " + v
}

// modelFilter is what the pane's filter form sends back.
type modelFilter struct {
	Q    string
	Pick map[string]string // facet key -> chosen value ("" = any)
}

func (f modelFilter) Empty() bool {
	if f.Q != "" {
		return false
	}
	for _, v := range f.Pick {
		if v != "" {
			return false
		}
	}
	return true
}

func (f modelFilter) keep(m probeModel, facets []facet) bool {
	if q := strings.ToLower(strings.TrimSpace(f.Q)); q != "" {
		hay := strings.ToLower(m.ID + " " + m.Name + " " + m.Desc)
		if !strings.Contains(hay, q) {
			return false
		}
	}
	for _, fc := range facets {
		want := f.Pick[fc.Key]
		if want == "" {
			continue
		}
		got, ok := m.Attrs[fc.Key]
		if !ok {
			return false
		}
		if fc.Numeric {
			if numOf(got) < numOf(want) {
				return false
			}
		} else if got != want {
			return false
		}
	}
	return true
}
