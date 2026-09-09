package main

import (
	"strings"
	"testing"
)

func TestProbeFacets(t *testing.T) {
	body := `{"data":[
	 {"id":"a","object":"model","displayName":"A","description":"long text","capabilities":{"tools":true,"vision":false,"maxContextTokens":262144},"availability":{"status":"available","checkedAt":"2026-01-01T00:00:01Z"},"rev":"h1"},
	 {"id":"b","object":"model","capabilities":{"tools":true,"vision":true,"maxContextTokens":524288},"availability":{"status":"unavailable","checkedAt":"2026-01-01T00:00:02Z"},"rev":"h2"},
	 {"id":"c","object":"model","capabilities":{"tools":false,"maxContextTokens":8192},"availability":{"status":"available","checkedAt":"2026-01-01T00:00:03Z"},"rev":"h3"}]}`
	ms := parseModels([]byte(body))
	fs := facetsOf(ms)
	var keys []string
	for _, f := range fs {
		keys = append(keys, f.Key)
	}
	// numeric first, then enums; per-model-unique rev/checkedAt are out
	if got := strings.Join(keys, ","); got != "capabilities.maxContextTokens,availability.status,capabilities.tools,capabilities.vision" {
		t.Fatalf("facets: %s", got)
	}
	if ms[0].Name != "A" || ms[0].Desc != "long text" || strings.Join(ms[0].Tags, "|") != "status: available|maxContextTokens 262k|tools" {
		t.Fatalf("model a: %+v", ms[0])
	}
	f := modelFilter{Pick: map[string]string{"capabilities.maxContextTokens": "262144", "capabilities.tools": "true"}}
	var kept []string
	for _, m := range ms {
		if f.keep(m, fs) {
			kept = append(kept, m.ID)
		}
	}
	if strings.Join(kept, ",") != "a,b" {
		t.Fatalf("kept %v", kept)
	}
	f = modelFilter{Q: "LONG"}
	if !f.keep(ms[0], fs) || f.keep(ms[1], fs) {
		t.Fatal("search should match description case-insensitively")
	}
	if fmtNum("524288") != "524k" || fmtNum("42") != "42" || fmtNum("1500000") != "1.5M" {
		t.Fatal("fmtNum")
	}
}
