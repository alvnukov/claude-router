package main

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"localrouter/internal/history"
	webui "localrouter/internal/ui"
)

// Completed history records are immutable. Cache the small measurement so UI
// polling does not repeatedly parse potentially large encrypted replay items.
// The cache is pruned to the current history window on every snapshot.
type connectionUsageCache struct {
	mu      sync.Mutex
	entries map[string]cachedConnectionUsage
}

type cachedConnectionUsage struct {
	end   time.Time
	usage tokenMeasurement
}

type tokenMeasurement struct {
	scope                                      string
	known, cacheKnown, reasoningKnown, invalid bool
	input, cached, written, output, reasoning  int64
	calls                                      int
}

type cacheSample struct {
	scope         string
	known         bool
	at            time.Time
	input, cached int64
}

type connectionUsageView struct {
	connections map[string]webui.ConnectionUsage
	sessions    map[string]webui.ConnectionUsage
}

func (c *connectionUsageCache) view(records []*history.Record, now time.Time, providers []provider) connectionUsageView {
	c.mu.Lock()
	defer c.mu.Unlock()
	retained := make(map[string]cachedConnectionUsage)
	out := connectionUsageView{connections: make(map[string]webui.ConnectionUsage), sessions: make(map[string]webui.ConnectionUsage)}
	codex := make(map[string]bool)
	for _, p := range providers {
		codex[p.Name] = p.Type == "codex"
	}
	groups := make(map[string]map[string][]cacheSample)
	since := now.Add(-24 * time.Hour)
	for _, rec := range records {
		if !rec.Done() || rec.Start.Before(since) || rec.Start.After(now) || (rec.Path != "" && rec.Path != "/v1/messages") {
			continue
		}
		name, _, _ := strings.Cut(rec.Served, "/")
		if rec.Route == "cloud" || rec.Route == "passthrough" {
			name = "anthropic"
		}
		if name == "" {
			continue
		}
		entry, ok := c.entries[rec.ID]
		if !ok || rec.ID == "" || !entry.end.Equal(rec.End) {
			entry = cachedConnectionUsage{end: rec.End, usage: measureTokens(rec)}
		}
		if rec.ID != "" {
			retained[rec.ID] = entry
		}
		m := entry.usage
		out.connections[name] = addMeasurement(out.connections[name], m, since)
		out.sessions[rec.Session] = addMeasurement(out.sessions[rec.Session], m, since)
		// Include unknown cache metadata in the series: it must break the
		// evidence chain instead of reviving an older low-cache warning.
		if codex[name] && rec.Session != "" && !rec.Failed() {
			if groups[name] == nil {
				groups[name] = make(map[string][]cacheSample)
			}
			key := rec.Session + "\x00" + rec.Served
			groups[name][key] = append(groups[name][key], cacheSample{scope: m.scope, at: rec.Start, input: m.input, cached: m.cached, known: m.known && m.cacheKnown})
		}
	}
	for name, sessions := range groups {
		for key, samples := range sessions {
			if len(samples) < 6 {
				continue
			}
			sort.Slice(samples, func(i, j int) bool { return samples[i].at.Before(samples[j].at) })
			// A changed account starts a fresh series, even if its first prompt
			// is short. Returning to an earlier account must not revive its old
			// warning. Keep all traffic in totals, but diagnose only this series.
			start := len(samples) - 1
			for start > 0 && samples[start-1].scope == samples[len(samples)-1].scope {
				start--
			}
			current := samples[start:]
			eligible := make([]cacheSample, 0, len(current))
			for _, sample := range current {
				if sample.input >= 8192 || !sample.known {
					eligible = append(eligible, sample)
				}
			}
			if len(eligible) < 6 {
				continue
			}
			// Examine the latest five after excluding the first cold request.
			var input, cached int64
			known := true
			for _, sample := range eligible[len(eligible)-5:] {
				known = known && sample.known
				input += sample.input
				cached += sample.cached
			}
			if known && input >= 100000 && float64(cached)/float64(input) < 0.20 {
				v := out.connections[name]
				v.LowCache = true
				out.connections[name] = v
				session, _, _ := strings.Cut(key, "\x00")
				s := out.sessions[session]
				s.LowCache = true
				out.sessions[session] = s
			}
		}
	}
	c.entries = retained
	return out
}

func addMeasurement(v webui.ConnectionUsage, m tokenMeasurement, since time.Time) webui.ConnectionUsage {
	v.Since = since
	v.Requests++
	if m.invalid {
		v.InvalidRequests++
	}
	if !m.known {
		return v
	}
	v.MeasuredRequests++
	v.InputTokens += m.input
	v.OutputTokens += m.output
	v.UpstreamCalls += m.calls
	if m.calls > 1 {
		v.ContinuationRequests++
	}
	if m.cacheKnown {
		v.CacheMeasuredRequests++
		v.CacheInputTokens += m.input
		v.CachedInputTokens += m.cached
		v.UncachedInputTokens += m.input - m.cached
		v.CacheWriteTokens += m.written
	}
	if m.reasoningKnown {
		v.ReasoningMeasuredRequests++
		v.ReasoningTokens += m.reasoning
	}
	return v
}

func measureTokens(rec *history.Record) tokenMeasurement {
	if len(rec.ProviderState) != 0 {
		// Never fall back to the final sampling's client-visible usage when
		// native multi-call accounting is present but incomplete.
		var state struct {
			Scope string `json:"scope"`
			Known bool   `json:"usage_known"`
			Calls int    `json:"calls"`
			Usage struct {
				Input        *int64 `json:"input_tokens"`
				Output       *int64 `json:"output_tokens"`
				InputDetails struct {
					Cached  *int64 `json:"cached_tokens"`
					Written *int64 `json:"cache_write_tokens"`
				} `json:"input_tokens_details"`
				OutputDetails struct {
					Reasoning *int64 `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal(rec.ProviderState, &state) != nil {
			return tokenMeasurement{}
		}
		if !state.Known {
			return tokenMeasurement{scope: state.Scope}
		}
		if state.Usage.Input == nil || state.Usage.Output == nil {
			return tokenMeasurement{scope: state.Scope, invalid: true}
		}
		m := tokenMeasurement{scope: state.Scope, known: true, input: *state.Usage.Input, output: *state.Usage.Output, calls: max(1, state.Calls)}
		if value := state.Usage.InputDetails.Cached; value != nil {
			m.cacheKnown = true
			m.cached = *value
		}
		if value := state.Usage.InputDetails.Written; value != nil {
			m.written = *value
		}
		if value := state.Usage.OutputDetails.Reasoning; value != nil {
			m.reasoningKnown = true
			m.reasoning = *value
		}
		return validMeasurement(m)
	}
	if rec.UsageKnown != nil && !*rec.UsageKnown {
		return tokenMeasurement{}
	}
	if rec.Resp == nil || rec.Failed() {
		return tokenMeasurement{}
	}
	u := rec.Resp.Usage
	input, inputKnown := u["input_tokens"]
	output, outputKnown := u["output_tokens"]
	if !inputKnown || !outputKnown {
		return tokenMeasurement{}
	}
	cached, cacheKnown := u["cache_read_input_tokens"]
	written := u["cache_creation_input_tokens"]
	// The old local adapter always emitted 0/0 when the upstream omitted usage.
	// Without provenance, that particular legacy shape cannot prove a free
	// request. Passthrough data and explicit upstream zero remain measurable.
	if rec.UsageKnown == nil && rec.Route == "local" && input == 0 && output == 0 && !cacheKnown && written == 0 {
		return tokenMeasurement{}
	}
	if input < 0 || output < 0 || cached < 0 || written < 0 {
		return tokenMeasurement{invalid: true}
	}
	// The captured response is Anthropic-shaped: its input excludes cache reads
	// and writes. Older adapter records without cache fields remain unknown.
	return validMeasurement(tokenMeasurement{known: true, cacheKnown: cacheKnown,
		input: int64(input) + int64(cached) + int64(written), cached: int64(cached), written: int64(written), output: int64(output), calls: 1})
}

func validMeasurement(m tokenMeasurement) tokenMeasurement {
	if m.input < 0 || m.output < 0 || m.cached < 0 || m.written < 0 || m.cached > m.input || m.written > m.input-m.cached || m.reasoning < 0 || m.reasoning > m.output {
		return tokenMeasurement{scope: m.scope, invalid: true}
	}
	return m
}
