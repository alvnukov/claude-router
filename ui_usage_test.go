package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"localrouter/internal/history"
	webui "localrouter/internal/ui"
)

func TestUIUsagePreservesUnknownAndNativeTotals(t *testing.T) {
	tests := []struct {
		name  string
		state string
		usage map[string]int
		want  tokenMeasurement
	}{
		{"native totals include continuations", `{"usage_known":true,"calls":2,"usage":{"input_tokens":12000,"output_tokens":80,"input_tokens_details":{"cached_tokens":9000,"cache_write_tokens":1000},"output_tokens_details":{"reasoning_tokens":60}}}`, map[string]int{"input_tokens": 10, "output_tokens": 2}, tokenMeasurement{known: true, cacheKnown: true, reasoningKnown: true, input: 12000, cached: 9000, written: 1000, output: 80, reasoning: 60, calls: 2}},
		{"native unknown never uses final sampling", `{"usage_known":false,"calls":2,"usage":{"input_tokens":12000,"output_tokens":80}}`, map[string]int{"input_tokens": 10, "output_tokens": 2}, tokenMeasurement{}},
		{"native cache absent", `{"usage_known":true,"calls":1,"usage":{"input_tokens":5000,"output_tokens":20}}`, nil, tokenMeasurement{known: true, input: 5000, output: 20, calls: 1}},
		{"native zero cache observed", `{"usage_known":true,"calls":1,"usage":{"input_tokens":5000,"output_tokens":20,"input_tokens_details":{"cached_tokens":0}}}`, nil, tokenMeasurement{known: true, cacheKnown: true, input: 5000, output: 20, calls: 1}},
		{"impossible native cache", `{"usage_known":true,"usage":{"input_tokens":5000,"output_tokens":20,"input_tokens_details":{"cached_tokens":6000}}}`, nil, tokenMeasurement{invalid: true}},
		{"impossible reasoning", `{"usage_known":true,"usage":{"input_tokens":5000,"output_tokens":20,"output_tokens_details":{"reasoning_tokens":40}}}`, nil, tokenMeasurement{invalid: true}},
		{"missing native counter", `{"usage_known":true,"usage":{"output_tokens":20}}`, nil, tokenMeasurement{invalid: true}},
		{"legacy absent cache", "", map[string]int{"input_tokens": 5000, "output_tokens": 20}, tokenMeasurement{known: true, input: 5000, output: 20, calls: 1}},
		{"anthropic gross reconstructed", "", map[string]int{"input_tokens": 1000, "output_tokens": 20, "cache_read_input_tokens": 3500, "cache_creation_input_tokens": 500}, tokenMeasurement{known: true, cacheKnown: true, input: 5000, cached: 3500, written: 500, output: 20, calls: 1}},
		{"invalid captured count", "", map[string]int{"input_tokens": -1, "output_tokens": 20}, tokenMeasurement{invalid: true}},
		{"absent output is unknown", "", map[string]int{"input_tokens": 1000}, tokenMeasurement{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &history.Record{ProviderState: json.RawMessage(tt.state), Resp: &history.Response{Usage: tt.usage}}
			if got := measureTokens(r); got != tt.want {
				t.Fatalf("measurement=%+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestUIUsageDistinguishesMeasuredZeroFromSyntheticUsage(t *testing.T) {
	known, unknown := true, false
	tests := []struct {
		name       string
		route      string
		provenance *bool
		usage      map[string]int
		native     string
		wantKnown  bool
	}{
		{"explicit upstream zero", "local", &known, map[string]int{"input_tokens": 0, "output_tokens": 0}, "", true},
		{"absent upstream usage", "local", &unknown, map[string]int{"input_tokens": 0, "output_tokens": 0}, "", false},
		{"incomplete upstream usage", "local", &unknown, map[string]int{"input_tokens": 100, "output_tokens": 0}, "", false},
		{"ambiguous legacy local zero", "local", nil, map[string]int{"input_tokens": 0, "output_tokens": 0}, "", false},
		{"legacy nonzero observed", "local", nil, map[string]int{"input_tokens": 100, "output_tokens": 1}, "", true},
		{"legacy cache zero observed", "local", nil, map[string]int{"input_tokens": 0, "output_tokens": 0, "cache_read_input_tokens": 0}, "", true},
		{"passthrough explicit zero", "cloud", nil, map[string]int{"input_tokens": 0, "output_tokens": 0}, "", true},
		{"native totals remain authoritative", "local", &unknown, map[string]int{"input_tokens": 0, "output_tokens": 0}, `{"usage_known":true,"usage":{"input_tokens":100,"output_tokens":2}}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &history.Record{Route: tt.route, UsageKnown: tt.provenance, Resp: &history.Response{Usage: tt.usage}, ProviderState: json.RawMessage(tt.native)}
			if got := measureTokens(rec); got.known != tt.wantKnown {
				t.Fatalf("known=%v, want %v: %+v", got.known, tt.wantKnown, got)
			}
		})
	}
}

func usageRecord(id, session, served string, at time.Time, input, cached int) *history.Record {
	usage := map[string]int{"input_tokens": input, "output_tokens": 100}
	if cached >= 0 {
		usage["cache_read_input_tokens"] = cached
		usage["input_tokens"] -= cached
	}
	return &history.Record{ID: id, Session: session, Served: served, Route: "local", Path: "/v1/messages", Start: at, End: at.Add(time.Second), Status: 200, Resp: &history.Response{Usage: usage}}
}

func TestUIUsageConnectionSessionWindowAndCoverage(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	records := []*history.Record{
		usageRecord("1", "one", "codex/model", now.Add(-time.Minute), 10000, 8000),
		usageRecord("2", "one", "codex/model", now.Add(-2*time.Minute), 20000, -1),
		usageRecord("3", "two", "other/model", now.Add(-3*time.Minute), 30000, 15000),
		usageRecord("old", "one", "codex/model", now.Add(-25*time.Hour), 99000, 0),
		usageRecord("future", "one", "codex/model", now.Add(time.Minute), 99000, 0),
		usageRecord("pending", "one", "codex/model", now.Add(-time.Minute), 99000, 0),
		usageRecord("not-model", "one", "codex/model", now.Add(-time.Minute), 99000, 0),
	}
	records[5].End = time.Time{}
	records[6].Path = "/v1/models"
	var cache connectionUsageCache
	view := cache.view(records, now, []provider{{Name: "codex", Type: "codex"}})
	v := view.connections["codex"]
	if v.Requests != 2 || v.MeasuredRequests != 2 || v.CacheMeasuredRequests != 1 || v.InputTokens != 30000 || v.CacheInputTokens != 10000 || v.CachedInputTokens != 8000 || v.UncachedInputTokens != 2000 || v.OutputTokens != 200 || v.LowCache {
		t.Fatalf("connection mixes data/window/unknown: %+v", v)
	}
	if s := view.sessions["one"]; s != v {
		t.Fatalf("session=%+v, connection=%+v", s, v)
	}
	if view.sessions["two"].InputTokens != 30000 || len(cache.entries) != 3 {
		t.Fatal("session isolation or cache pruning failed")
	}
	if next := cache.view(records, now.Add(24*time.Hour), nil); len(next.connections) != 1 || len(cache.entries) != 1 {
		t.Fatal("expired measurements retained")
	}
}

func TestUIUsageLowCacheNeedsRepeatedMeasuredLargeCodexRequests(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name                             string
		count, input, cached             int
		separateSessions, separateModels bool
		kind                             string
		warn                             bool
	}{
		{"cold first request", 1, 200000, 0, false, false, "codex", false},
		{"small prompts", 9, 8000, 0, false, false, "codex", false},
		{"cache metadata absent", 8, 30000, -1, false, false, "codex", false},
		{"different sessions", 8, 30000, 0, true, false, "codex", false},
		{"different models", 8, 30000, 0, false, true, "codex", false},
		{"healthy", 8, 30000, 25000, false, false, "codex", false},
		{"threshold healthy", 8, 30000, 6000, false, false, "codex", false},
		{"repeated low cache", 6, 30000, 1000, false, false, "codex", true},
		{"generic opt-in caching", 8, 30000, 0, false, false, "openai", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var records []*history.Record
			for i := range tt.count {
				session, model := "session", "codex/model"
				if tt.separateSessions {
					session = fmt.Sprint(i)
				}
				if tt.separateModels {
					model += fmt.Sprint(i)
				}
				records = append(records, usageRecord(fmt.Sprint(i), session, model, now.Add(-time.Duration(i+1)*time.Minute), tt.input, tt.cached))
			}
			var cache connectionUsageCache
			v := cache.view(records, now, []provider{{Name: "codex", Type: tt.kind}})
			if v.connections["codex"].LowCache != tt.warn || (!tt.separateSessions && v.sessions["session"].LowCache != tt.warn) {
				t.Fatalf("warning: %+v", v)
			}
		})
	}
}

func TestUIUsageLatestSeriesRecoversAndInvalidDataExcluded(t *testing.T) {
	now := time.Now()
	var records []*history.Record
	for i := range 12 {
		cached := 0
		if i >= 7 {
			cached = 28000
		}
		records = append(records, usageRecord(fmt.Sprint(i), "session", "codex/model", now.Add(-time.Duration(12-i)*time.Minute), 30000, cached))
	}
	bad := usageRecord("bad", "session", "codex/model", now.Add(-time.Second), 30000, 0)
	bad.ProviderState = json.RawMessage(`{"usage_known":true,"usage":{"input_tokens":5,"output_tokens":1,"input_tokens_details":{"cached_tokens":6}}}`)
	records = append(records, bad)
	var cache connectionUsageCache
	v := cache.view(records, now, []provider{{Name: "codex", Type: "codex"}}).connections["codex"]
	if v.LowCache || v.InvalidRequests != 1 || v.MeasuredRequests != 12 || v.Requests != 13 || v.InputTokens != 360000 {
		t.Fatalf("latest healthy series did not recover: %+v", v)
	}
}

func TestUIUsageUnknownCacheAndAccountChangeBreakWarningSeries(t *testing.T) {
	now := time.Now()
	for _, changedAccount := range []bool{false, true} {
		var records []*history.Record
		for i := range 6 {
			r := usageRecord(fmt.Sprint(i), "session", "codex/model", now.Add(-time.Duration(6-i)*time.Minute), 30000, 0)
			if changedAccount {
				scope := "account-before"
				if i == 5 {
					scope = "account-after"
				}
				r.ProviderState = json.RawMessage(fmt.Sprintf(`{"scope":%q,"usage_known":true,"usage":{"input_tokens":30000,"output_tokens":100,"input_tokens_details":{"cached_tokens":0}}}`, scope))
			} else if i == 5 {
				delete(r.Resp.Usage, "cache_read_input_tokens")
			}
			records = append(records, r)
		}
		var cache connectionUsageCache
		if v := cache.view(records, now, []provider{{Name: "codex", Type: "codex"}}).connections["codex"]; v.LowCache {
			t.Fatalf("unproven low-cache warning across changed account=%v", changedAccount)
		}
	}
}

func TestUIUsageScopeChangeClearsPreviouslyEstablishedWarning(t *testing.T) {
	now := time.Now()
	for _, returnToOriginalAccount := range []bool{false, true} {
		t.Run(fmt.Sprintf("return=%v", returnToOriginalAccount), func(t *testing.T) {
			var records []*history.Record
			for i := range 6 {
				r := usageRecord(fmt.Sprint(i), "session", "codex/model", now.Add(-time.Duration(10-i)*time.Minute), 30000, 0)
				r.ProviderState = json.RawMessage(`{"scope":"account-before","usage_known":true,"usage":{"input_tokens":30000,"output_tokens":100,"input_tokens_details":{"cached_tokens":0}}}`)
				records = append(records, r)
			}
			var cache connectionUsageCache
			if !cache.view(records, now, []provider{{Name: "codex", Type: "codex"}}).connections["codex"].LowCache {
				t.Fatal("fixture must establish an actual low-cache warning first")
			}
			latest := usageRecord("new-account", "session", "codex/model", now.Add(-2*time.Minute), 30000, 28000)
			latest.ProviderState = json.RawMessage(`{"scope":"account-after","usage_known":true,"usage":{"input_tokens":30000,"output_tokens":100,"input_tokens_details":{"cached_tokens":28000}}}`)
			records = append(records, latest)
			if returnToOriginalAccount {
				returned := usageRecord("returned-account", "session", "codex/model", now.Add(-time.Minute), 30000, 28000)
				returned.ProviderState = json.RawMessage(`{"scope":"account-before","usage_known":true,"usage":{"input_tokens":30000,"output_tokens":100,"input_tokens_details":{"cached_tokens":28000}}}`)
				records = append(records, returned)
			}
			view := cache.view(records, now, []provider{{Name: "codex", Type: "codex"}})
			if view.connections["codex"].LowCache || view.sessions["session"].LowCache {
				t.Fatal("previous account's warning survived a fresh sampling series")
			}
			if view.connections["codex"].MeasuredRequests != len(records) {
				t.Fatal("clearing a stale warning must retain measured traffic totals")
			}
		})
	}
}

func TestUIJSONUsageDoesNotExposeReplayAndSessionIgnoresPagination(t *testing.T) {
	u, h := testUI(t)
	now := time.Now()
	for i := range 3 {
		r := usageRecord(fmt.Sprint(i), "usage-session", "p/m1", now.Add(-time.Duration(i+1)*time.Minute), 10000, 8000)
		r.ProviderState = json.RawMessage(`{"usage_known":true,"calls":2,"scope":"PRIVATE-SCOPE","output":[{"encrypted_content":"PRIVATE-REASONING"}],"usage":{"input_tokens":25000,"output_tokens":300,"input_tokens_details":{"cached_tokens":20000},"output_tokens_details":{"reasoning_tokens":100}}}`)
		u.st.Add(r)
	}
	stateResponse := apiCall(t, h, "GET", "/api/ui/state", nil)
	var state webui.State
	if err := json.Unmarshal(stateResponse.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if v := state.Connections[1].Usage; v.InputTokens != 75000 || v.CachedInputTokens != 60000 || v.ContinuationRequests != 3 {
		t.Fatalf("native sums lost: %+v", v)
	}
	for _, path := range []string{"/api/ui/state", "/api/ui/requests?session=usage-session&limit=1&offset=1&errors=1"} {
		response := apiCall(t, h, "GET", path, nil)
		for _, secret := range []string{"PRIVATE-SCOPE", "PRIVATE-REASONING", "encrypted_content", "providerState"} {
			if strings.Contains(response.Body.String(), secret) {
				t.Fatalf("%s exposed replay", path)
			}
		}
		if strings.Contains(path, "requests?") {
			var list webui.RequestList
			if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
				t.Fatal(err)
			}
			if list.Total != 0 || list.SessionUsage == nil || list.SessionUsage.InputTokens != 75000 || list.SessionUsage.Requests != 3 {
				t.Fatalf("filter/pagination changed session totals: %+v", list)
			}
		}
	}
}
