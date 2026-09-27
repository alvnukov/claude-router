package codex

import (
	"strings"
	"testing"
)

func TestReadDistinguishesMissingUsageFromMeasuredZero(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		known bool
	}{
		{`null`, false}, {`{}`, false}, {`{"input_tokens":12}`, false},
		{`{"input_tokens":0,"output_tokens":0}`, true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			result, err := Read(strings.NewReader(contractSSE(contractCompleted("r", `,"usage":`+tc.raw))), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if result.UsageKnown != tc.known {
				t.Fatalf("UsageKnown=%v want %v", result.UsageKnown, tc.known)
			}
		})
	}
}
