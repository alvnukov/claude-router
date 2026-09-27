package regression_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type routeCase struct {
	Profile     string `json:"profile"`
	Model       string `json:"model"`
	Effort      string `json:"effort"`
	Status      int    `json:"status"`
	Destination string `json:"destination"`
	ServedModel string `json:"served_model"`
}

func routeCases(t *testing.T) []routeCase {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "routes", "route-matrix.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []routeCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 8 {
		t.Fatalf("fixture matrix changed without review: %d rows, want 8", len(cases))
	}
	return cases
}

func routeExpectation(row routeCase) ([]expectedCall, []expectedCall, string) {
	if row.Status != http.StatusOK {
		return nil, nil, ""
	}
	call := expectedCall{Path: "/v1/chat/completions", Model: row.ServedModel}
	if row.Destination == "fixture-a" {
		return []expectedCall{call}, nil, "OK-A"
	}
	return nil, []expectedCall{call}, "OK-B"
}

func checkRoutedMessage(observed []byte, model, text string) error {
	value, err := parseJSON(observed)
	if err != nil {
		return err
	}
	message, ok := value.(map[string]any)
	if !ok || message["type"] != "message" || message["role"] != "assistant" || message["model"] != model || message["stop_reason"] != "end_turn" {
		return fmt.Errorf("wrong Anthropic message type/role/model/stop: %v", message)
	}
	blocks, ok := message["content"].([]any)
	if !ok || len(blocks) != 1 {
		return fmt.Errorf("expected exactly one text block: %v", message["content"])
	}
	block, ok := blocks[0].(map[string]any)
	if !ok || !reflect.DeepEqual(block, map[string]any{"type": "text", "text": text}) {
		return fmt.Errorf("wrong text block: %v", blocks[0])
	}
	return nil
}

func TestRegressionRouteMatrix(t *testing.T) {
	for _, row := range routeCases(t) {
		name := fmt.Sprintf("RR-RTE-01/%s/%s/%s", row.Profile, row.Model, row.Effort)
		t.Run(name, func(t *testing.T) {
			stand := startRouter(t, testFixture{profile: row.Profile})
			wantA, wantB, wantText := routeExpectation(row)
			if row.Status == http.StatusOK && wantA == nil && wantB == nil {
				t.Fatalf("unknown manually specified destination %q", row.Destination)
			}
			status, body := stand.clientCall(t, "/v1/messages", requestWithEffort(row.Model, row.Effort, "route-fixture", false))
			if status != row.Status {
				t.Errorf("%s: HTTP status %d, expected %d", name, status, row.Status)
			}
			if status == http.StatusOK {
				if err := checkRoutedMessage(body, row.Model, wantText); err != nil {
					t.Errorf("%s: client body: %v", name, err)
				}
			} else if len(body) == 0 {
				t.Error("disabled route returned no error body")
			}
			if err := compareCalls(stand.a.allCalls(), wantA); err != nil {
				t.Errorf("%s: fixture A: %v", name, err)
			}
			if err := compareCalls(stand.b.allCalls(), wantB); err != nil {
				t.Errorf("%s: fixture B: %v", name, err)
			}
			if calls := stand.cloud.allCalls(); len(calls) != 0 {
				t.Errorf("%s: forbidden cloud upstream called %d times", name, len(calls))
			}
		})
	}
}

func TestRegressionEffortMappingAndAbsentMapping(t *testing.T) {
	stand := startRouter(t, testFixture{setup: func(a, b *upstreamStub) map[string]any {
		cfg := setupProfiles(a, b)
		red := cfg["profiles"].(map[string]any)["rr-red"].(map[string]any)
		red["model_pools"].(map[string]any)["b"] = []map[string]any{{
			"model": "fixture-b/fixture-b-model",
			"effort_map": map[string]string{"high": "high"},
		}}
		return cfg
	}})
	for _, row := range []struct{ effort, text string }{{"high", "OK-B"}, {"default", "OK-A"}} {
		status, body := stand.clientCall(t, "/v1/messages", requestWithEffort("claude-sonnet-4-5-20250929", row.effort, "effort-fixture", false))
		if status != http.StatusOK {
			t.Fatalf("RR-RTE-01: %s status %d", row.effort, status)
		}
		if err := checkRoutedMessage(body, "claude-sonnet-4-5-20250929", row.text); err != nil {
			t.Error(err)
		}
	}
	if err := compareCalls(stand.b.allCalls(), []expectedCall{{
		Path: "/v1/chat/completions", Model: "fixture-b-model", Effort: "high",
		Body: []byte(`{"model":"fixture-b-model","messages":[{"role":"user","content":"READY-T"}],"max_tokens":64,"reasoning_effort":"high"}`),
	}}); err != nil {
		t.Errorf("RR-RTE-01: explicit effort map did not reach B: %v", err)
	}
	if err := compareCalls(stand.a.allCalls(), []expectedCall{{Path: "/v1/chat/completions", Model: "fixture-a-model", Effort: ""}}); err != nil {
		t.Errorf("RR-RTE-01: absent mapping forwarded effort or chose wrong destination: %v", err)
	}
	if len(stand.cloud.allCalls()) != 0 {
		t.Error("RR-RTE-01: local effort request reached cloud")
	}
}

func TestRegressionRouteOracleRejectsWrongDestinationAndExtraCall(t *testing.T) {
	row := routeCases(t)[3] // fixed high/version override: fixture B, not family fixture A.
	wantA, wantB, _ := routeExpectation(row)
	if err := compareCalls(nil, wantA); err != nil {
		t.Fatal(err)
	}
	good := []observedCall{{Path: "/v1/chat/completions", Model: "fixture-b-model"}}
	if err := compareCalls(good, wantB); err != nil {
		t.Fatal(err)
	}
	for name, broken := range map[string][]observedCall{
		"wrong_model_same_text": {{Path: "/v1/chat/completions", Model: "fixture-a-model"}},
		"extra_attempt":         {{Path: "/v1/chat/completions", Model: "fixture-b-model"}, {Path: "/v1/chat/completions", Model: "fixture-b-model"}},
		"extra_other_endpoint":  {{Path: "/v1/chat/completions", Model: "fixture-b-model"}, {Path: "/v1/models", Model: "fixture-b-model"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := compareCalls(broken, wantB); err == nil {
				t.Error("altered observed journal passed fixed destination oracle")
			}
		})
	}
	if err := compareCalls([]observedCall{{Path: "/v1/chat/completions", Model: "fixture-a-model"}}, wantA); err == nil {
		t.Error("forbidden A call passed zero-call oracle")
	}
	if err := checkRoutedMessage([]byte(`{"type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929","stop_reason":"end_turn","content":[{"type":"text","text":"OK-A"}]}`), row.Model, "OK-B"); err == nil {
		t.Error("wrong client text passed fixed oracle")
	}
}

func TestRegressionCloudPassthroughAndDisabled(t *testing.T) {
	stand := startRouter(t, testFixture{setup: func(a, b *upstreamStub) map[string]any {
		cfg := setupProfiles(a, b)
		red := cfg["profiles"].(map[string]any)["rr-red"].(map[string]any)
		red["routes"].(map[string]any)["claude-sonnet-4-5-20250929"].(map[string]any)["high"] = map[string]string{"mode": "anthropic"}
		return cfg
	}})
	body, err := os.ReadFile(filepath.Join("testdata", "wire", "cloud-request.json"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := os.ReadFile(filepath.Join("testdata", "wire", "cloud-response.json"))
	if err != nil {
		t.Fatal(err)
	}
	stand.cloud.setReply(http.StatusOK, "application/json", response)
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	request["output_config"] = map[string]string{"effort": "high"}
	body, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	status, got := stand.clientCall(t, "/v1/messages", body)
	if status != http.StatusOK {
		t.Fatalf("RR-CLOUD-01: status %d, want 200", status)
	}
	if err := compareJSON(got, response); err != nil {
		t.Errorf("RR-CLOUD-01: client message: %v", err)
	}
	calls := stand.cloud.allCalls()
	if len(calls) != 1 || calls[0].Path != "/v1/messages" || !bytes.Equal(calls[0].Body, body) {
		t.Errorf("RR-CLOUD-01: cloud did not receive exactly the original byte sequence at /v1/messages: calls=%d", len(calls))
	}
	if len(stand.a.allCalls()) != 0 || len(stand.b.allCalls()) != 0 {
		t.Error("RR-CLOUD-01: cloud request also called a local candidate")
	}
	before := len(stand.cloud.allCalls())
	status, _ = stand.clientCall(t, "/v1/messages", requestWithEffort("claude-sonnet-4-5-20250929", "max", "cloud-fixture", false))
	if status != http.StatusBadRequest || len(stand.cloud.allCalls()) != before {
		t.Errorf("RR-CLOUD-01: disabled route returned status %d or called cloud", status)
	}
}

func TestRegressionCloudOracleRejectsCorruptedObservedBody(t *testing.T) {
	fixed := []byte(`{"type":"message","content":[{"type":"text","text":"OK-T"}],"stop_reason":"end_turn"}`)
	if err := compareJSON(fixed, fixed); err != nil {
		t.Fatal(err)
	}
	altered := bytes.Replace(fixed, []byte("OK-T"), []byte("OK-X"), 1)
	if err := compareJSON(altered, fixed); err == nil {
		t.Error("corrupt observed cloud response passed fixed oracle")
	}
}

func TestRegressionProfileSwitchAndRestart(t *testing.T) {
	stand := startRouter(t, testFixture{})
	input := requestWithEffort("claude-sonnet-4-5-20250929", "default", "same-session", false)
	status, body := stand.clientCall(t, "/v1/messages", input)
	if status != http.StatusOK || checkRoutedMessage(body, "claude-sonnet-4-5-20250929", "OK-A") != nil {
		t.Fatalf("RR-PRO-01: initial red profile failed: status %d", status)
	}
	stand.activateProfile(t, "rr-blue")
	status, body = stand.clientCall(t, "/v1/messages", input)
	if status != http.StatusOK {
		t.Fatalf("RR-PRO-01: blue profile returned %d", status)
	}
	if err := checkRoutedMessage(body, "claude-sonnet-4-5-20250929", "OK-B"); err != nil {
		t.Fatal(err)
	}
	if err := compareCalls(stand.a.allCalls(), []expectedCall{{Path: "/v1/chat/completions", Model: "fixture-a-model"}}); err != nil {
		t.Errorf("RR-PRO-01: A after switch: %v", err)
	}
	if err := compareCalls(stand.b.allCalls(), []expectedCall{{Path: "/v1/chat/completions", Model: "fixture-b-model"}}); err != nil {
		t.Errorf("RR-PRO-01: B after switch: %v", err)
	}
	status, state := stand.uiCall(t, http.MethodGet, "/api/ui/state", nil, "")
	if status != http.StatusOK || observedMessage(t, state)["activeProfile"] != "rr-blue" {
		t.Errorf("RR-PRO-01: UI state does not expose the selected profile: status %d", status)
	}
	stand.restart(t)
	status, state = stand.uiCall(t, http.MethodGet, "/api/ui/state", nil, "")
	if status != http.StatusOK || observedMessage(t, state)["activeProfile"] != "rr-blue" {
		t.Errorf("RR-PRO-02: a new router process did not load blue profile: status %d", status)
	}
	status, body = stand.clientCall(t, "/v1/messages", input)
	if status != http.StatusOK {
		t.Fatalf("RR-PRO-02: after restart status %d", status)
	}
	if err := checkRoutedMessage(body, "claude-sonnet-4-5-20250929", "OK-B"); err != nil {
		t.Error(err)
	}
	if err := compareCalls(stand.a.allCalls(), []expectedCall{{Path: "/v1/chat/completions", Model: "fixture-a-model"}}); err != nil {
		t.Errorf("RR-PRO-02: forbidden A after restart: %v", err)
	}
	if err := compareCalls(stand.b.allCalls(), []expectedCall{{Path: "/v1/chat/completions", Model: "fixture-b-model"}, {Path: "/v1/chat/completions", Model: "fixture-b-model"}}); err != nil {
		t.Errorf("RR-PRO-02: restart route not retained: %v", err)
	}
}

func TestRegressionPoolAttemptOrderAndAffinity(t *testing.T) {
	stand := startRouter(t, testFixture{setup: func(a, b *upstreamStub) map[string]any {
		cfg := setupProfiles(a, b)
		red := cfg["profiles"].(map[string]any)["rr-red"].(map[string]any)
		red["model_pools"].(map[string]any)["a"] = []map[string]string{
			{"model": "fixture-a/fixture-a-model"}, {"model": "fixture-b/fixture-b-model"},
		}
		cfg["pool_settings"] = map[string]any{"a": map[string]any{"type": "failover", "failover": true, "first_byte_seconds": 5}}
		red["pool_settings"] = cfg["pool_settings"]
		return cfg
	}})
	stand.a.setReply(http.StatusTooManyRequests, "application/json", []byte(`{"error":"synthetic unavailable"}`))
	stand.b.setReply(http.StatusOK, "application/json", []byte(`{"choices":[{"message":{"content":"OK-B"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`))
	input := requestWithEffort("claude-sonnet-4-5-20250929", "default", "affinity-session", false)
	for i := 0; i < 2; i++ {
		status, body := stand.clientCall(t, "/v1/messages", input)
		if status != http.StatusOK {
			t.Fatalf("RR-POOL-01/02: request %d returned %d", i, status)
		}
		if err := checkRoutedMessage(body, "claude-sonnet-4-5-20250929", "OK-B"); err != nil {
			t.Errorf("RR-POOL-01/02: request %d: %v", i, err)
		}
	}
	if err := compareCalls(stand.a.allCalls(), []expectedCall{{Path: "/v1/chat/completions", Model: "fixture-a-model"}}); err != nil {
		t.Errorf("RR-POOL-01: first failed attempt: %v", err)
	}
	if err := compareCalls(stand.b.allCalls(), []expectedCall{{Path: "/v1/chat/completions", Model: "fixture-b-model"}, {Path: "/v1/chat/completions", Model: "fixture-b-model"}}); err != nil {
		t.Errorf("RR-POOL-02: affinity to B: %v", err)
	}
	if err := compareAttemptOrder(stand.order.snapshot(), []string{"fixture-a/fixture-a-model", "fixture-b/fixture-b-model", "fixture-b/fixture-b-model"}); err != nil {
		t.Errorf("RR-POOL-01/02: %v", err)
	}
}

func TestRegressionPoolOracleRejectsExtraRetryAndReordering(t *testing.T) {
	fixed := []string{"fixture-a/fixture-a-model", "fixture-b/fixture-b-model"}
	if err := compareAttemptOrder(append([]string(nil), fixed...), fixed); err != nil {
		t.Fatal(err)
	}
	for _, corruptedObserved := range [][]string{
		{"fixture-b/fixture-b-model", "fixture-a/fixture-a-model"},
		{"fixture-a/fixture-a-model", "fixture-b/fixture-b-model", "fixture-a/fixture-a-model"},
	} {
		if err := compareAttemptOrder(corruptedObserved, fixed); err == nil {
			t.Error("corrupted observed attempt order passed fixed oracle")
		}
	}
}
