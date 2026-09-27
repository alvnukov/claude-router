package main

import (
	"encoding/json"
	"testing"
	"time"

	"localrouter/internal/history"
	webui "localrouter/internal/ui"
)

func TestUIJSONRecordedEfforts(t *testing.T) {
	u, h := testUI(t)
	for _, tc := range []struct {
		name, sent string
		want       *string
	}{
		{"openai", `{"model":"m1","reasoning_effort":"xhigh"}`, newString("xhigh")},
		{"codex", `{"model":"m1","reasoning":{"effort":"low"}}`, newString("low")},
		{"default", `{"model":"m1"}`, newString("")},
		{"unknown", "", nil},
		{"truncated", `{"model":"m1",`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &history.Record{Start: time.Now(), Model: "claude-sonnet-5", Route: "local", Served: "p/m1", ReqBody: []byte(`{"model":"claude-sonnet-5","output_config":{"effort":"high"}}`), OpenAIBody: []byte(tc.sent)}
			u.st.Add(r)
			w := apiCall(t, h, "GET", "/api/ui/requests/"+r.ID, nil)
			var detail webui.Detail
			if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
				t.Fatal(err)
			}
			if detail.RequestedEffort == nil || *detail.RequestedEffort != "high" {
				t.Fatal("missing incoming effort")
			}
			if tc.want == nil {
				if detail.SentEffort != nil {
					t.Fatal("fabricated sent effort")
				}
			} else if detail.SentEffort == nil || *detail.SentEffort != *tc.want {
				t.Fatalf("sent effort: %v", detail.SentEffort)
			}
		})
	}
}

func newString(value string) *string { return &value }
