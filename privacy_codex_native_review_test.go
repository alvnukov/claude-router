package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestPrivacyCodexRejectsInvalidFinalNativeProjection(t *testing.T) {
	item := `{"type":"function_call","id":"item1","call_id":"call1","name":"run","arguments":"{\"command\":\"echo safe\"}"}`
	changed := strings.Replace(item, "echo safe", "<secret:credential:deadbeef>", 1)
	for name, final := range map[string]string{
		"changed tool":  `{"type":"response.completed","response":{"id":"r1","output":[` + changed + `]}}`,
		"invalid usage": `{"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":9223372036854775808,"output_tokens":1}}}`,
	} {
		for _, stream := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/json", true: "/stream"}[stream], func(t *testing.T) {
				seedTwoConnections(t)
				old := http.DefaultTransport
				t.Cleanup(func() { http.DefaultTransport = old })
				response := "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":" + item + "}\n\ndata: " + final + "\n\n"
				calls := 0
				http.DefaultTransport = usageTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					return usageResponse(200, response), nil
				})
				body := trafficBody
				if stream {
					body = strings.Replace(body, `"max_tokens":100`, `"max_tokens":100,"stream":true`, 1)
				}
				w := trafficCall(privateCodexHandler(t), "/v1/messages", body)
				if w.Code != http.StatusBadGateway || calls != 1 {
					t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body)
				}
				for _, forbidden := range []string{"message_start", "tool_use", "echo safe", "deadbeef"} {
					if strings.Contains(w.Body.String(), forbidden) {
						t.Fatalf("invalid final projection released response content %q: %s", forbidden, w.Body)
					}
				}
			})
		}
	}
}
