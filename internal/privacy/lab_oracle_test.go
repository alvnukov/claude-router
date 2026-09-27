package privacy

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
)

const (
	labOracleCanary = "RR-LAB-SYN-7319"
	labOracleRules  = `{"fields":[{"path":"messages[*].content[*].input.password","kind":"secret"}]}`
	labOracleJSON   = `{"system":"keep-sentinel","model":"claude-synthetic","messages":[{"role":"user","content":[{"type":"text","text":"Привет \"мир\"\nkeep-sentinel"},{"type":"tool_use","id":"tool_1","name":"connect","input":{"password":"RR-LAB-SYN-7319","port":22}}]}],"metadata":{"user_id":"rr-lab-test","billing":"credits-7"}}`
)

// These observers take hand-specified expectations, never an expected value
// produced by Mask, Preview or Detect. Reasons contain no submitted content.
func labOracleText(output, canary string, required ...string) string {
	if output == "" {
		return "empty"
	}
	if strings.Contains(output, canary) {
		return "leak"
	}
	for _, s := range required {
		if !strings.Contains(output, s) {
			return "missing-allowed-text"
		}
	}
	return ""
}

func labOracleContains(value any, canary string) bool {
	switch v := value.(type) {
	case string:
		return strings.Contains(v, canary)
	case []any:
		for _, child := range v {
			if labOracleContains(child, canary) {
				return true
			}
		}
	case map[string]any:
		for key, child := range v {
			if strings.Contains(key, canary) || labOracleContains(child, canary) {
				return true
			}
		}
	}
	return false
}

func labOracleJSONProtected(output string, forbidden ...string) string {
	if output == "" {
		return "empty"
	}
	var root map[string]any
	if json.Unmarshal([]byte(output), &root) != nil || root == nil {
		return "invalid-json"
	}
	for _, canary := range forbidden {
		if strings.Contains(output, canary) || labOracleContains(root, canary) {
			return "leak"
		}
	}
	system, ok := root["system"].(string)
	if !ok || root["model"] != "claude-synthetic" {
		return "missing-allowed-text"
	}
	if system != "keep-sentinel" {
		parts := strings.Fields(system)
		if len(parts) != 4 || parts[0] != "keep-sentinel" || !strings.Contains(parts[2], "@") || parts[3] == "" {
			return "missing-allowed-text"
		}
		ip, err := netip.ParseAddr(parts[1])
		if err != nil || !netip.MustParsePrefix("100.64.0.0/10").Contains(ip) {
			return "invalid-ip-range"
		}
	}
	meta, ok := root["metadata"].(map[string]any)
	if !ok || len(meta) != 2 || meta["user_id"] != "rr-lab-test" || meta["billing"] != "credits-7" {
		return "metadata-structure"
	}
	messages, ok := root["messages"].([]any)
	if !ok || len(messages) != 1 {
		return "message-structure"
	}
	message, ok := messages[0].(map[string]any)
	if !ok || message["role"] != "user" {
		return "message-structure"
	}
	blocks, ok := message["content"].([]any)
	if !ok || len(blocks) != 2 {
		return "tool-structure"
	}
	text, ok := blocks[0].(map[string]any)
	if !ok || text["type"] != "text" || text["text"] != "Привет \"мир\"\nkeep-sentinel" {
		return "missing-allowed-text"
	}
	tool, ok := blocks[1].(map[string]any)
	if !ok || tool["type"] != "tool_use" || tool["id"] != "tool_1" || tool["name"] != "connect" {
		return "tool-structure"
	}
	input, ok := tool["input"].(map[string]any)
	if !ok || len(input) != 2 || input["port"] != float64(22) {
		return "tool-structure"
	}
	password, ok := input["password"].(string)
	if !ok || password == "" {
		return "tool-structure"
	}
	return ""
}

func labOracleForeignRestore(result PreviewResult, err error, foreign string) string {
	if strings.Contains(result.Output, foreign) {
		return "foreign-real"
	}
	if err != nil && result.Output != "" {
		return "partial-output"
	}
	return ""
}

func labOraclePreview(t *testing.T, lab *Lab, in PreviewInput) PreviewResult {
	t.Helper()
	result, err := lab.Preview(context.Background(), in)
	if err != nil {
		t.Fatalf("preview rejected synthetic input: %v", err)
	}
	if result.ID == "" {
		t.Fatal("preview omitted restoration capability")
	}
	t.Cleanup(func() { lab.Clear(result.ID) })
	return result
}

func TestLabOracleTextMaskRestore(t *testing.T) {
	const source = "password=RR-LAB-SYN-7319 keep-sentinel password_hint=demo"
	const allowed = "password_hint=demo"
	lab := NewLab(t.TempDir())
	in := PreviewInput{Mode: "text", Operation: ModeMask, Input: source, Rules: json.RawMessage(`{}`), Enabled: true}
	if got := labOracleText(source, labOracleCanary, "keep-sentinel", allowed); got != "leak" {
		t.Fatalf("raw positive control: %s", got)
	}
	for _, tc := range []struct{ output, reason string }{
		{"", "empty"},
		{"password=RR-LAB-SYN-7319 keep-sentinel password_hint=demo", "leak"},
		{"<secret:synthetic> password_hint=demo", "missing-allowed-text"},
	} {
		if got := labOracleText(tc.output, labOracleCanary, "keep-sentinel", allowed); got != tc.reason {
			t.Fatalf("corrupted local observer control: got %s, want %s", got, tc.reason)
		}
	}
	result := labOraclePreview(t, lab, in)
	if result.Operation != ModeMask || !result.Enabled || !result.Roundtrip || result.Masked[KindSecret] < 1 {
		t.Fatal("protected preview not executed or masking not reported")
	}
	if got := labOracleText(result.Output, labOracleCanary, "keep-sentinel", allowed); got != "" {
		t.Fatalf("protected text: %s", got)
	}
	back, err := lab.Restore(context.Background(), result.ID, result.Output)
	if err != nil || back.Output != source {
		t.Fatal("authorized restore did not return exact source", err)
	}
	state, err := json.Marshal(lab.State())
	if err != nil || strings.Contains(string(state), labOracleCanary) || strings.Contains(string(state), result.ID) || strings.Contains(string(state), result.Output) {
		t.Fatal("aggregate state retained input, output or capability", err)
	}

	detect := in
	detect.Operation = ModeDetect
	found, err := lab.Preview(context.Background(), detect)
	if err != nil || found.Output != source || found.ID != "" || found.Roundtrip || found.Detected[KindSecret] < 1 {
		t.Fatal("detect source/count/handle contract", err)
	}
	disabled := in
	disabled.Enabled = false
	bypass := labOraclePreview(t, lab, disabled)
	if bypass.Output != source || bypass.Enabled || bypass.Masked[KindSecret] != 0 {
		t.Fatal("disabled preview must remain a raw bypass")
	}
	// Neither detect nor disabled output is a protected model-visible pass.
}

func TestLabOracleJSONStructure(t *testing.T) {
	lab := NewLab(t.TempDir())
	in := PreviewInput{Mode: "json", Operation: ModeMask, Input: labOracleJSON, Rules: json.RawMessage(labOracleRules), Enabled: true}
	if got := labOracleJSONProtected(labOracleJSON, labOracleCanary); got != "leak" {
		t.Fatalf("decoded raw positive control: %s", got)
	}
	safe := strings.Replace(labOracleJSON, labOracleCanary, "<secret:synthetic>", 1)
	if got := labOracleJSONProtected(safe, labOracleCanary); got != "" {
		t.Fatalf("independent safe control: %s", got)
	}
	escaped := strings.Replace(labOracleJSON, labOracleCanary, string([]byte{92, 117, 48, 48, 53, 50, 92, 117, 48, 48, 53, 50})+"-LAB-SYN-7319", 1)
	if strings.Contains(escaped, labOracleCanary) || labOracleJSONProtected(escaped, labOracleCanary) != "leak" {
		t.Fatal("escaped canary positive control went undetected")
	}
	for _, tc := range []struct{ output, reason string }{
		{"", "empty"},
		{strings.Replace(safe, "<secret:synthetic>", labOracleCanary, 1), "leak"},
		{strings.Replace(safe, `,"port":22`, "", 1), "tool-structure"},
		{strings.Replace(safe, `,{"type":"tool_use","id":"tool_1","name":"connect","input":{"password":"<secret:synthetic>","port":22}}`, "", 1), "tool-structure"},
	} {
		if got := labOracleJSONProtected(tc.output, labOracleCanary); got != tc.reason {
			t.Fatalf("corrupted local JSON observer control: got %s, want %s", got, tc.reason)
		}
	}
	result := labOraclePreview(t, lab, in)
	if !result.Enabled || !result.Roundtrip || result.Masked[KindSecret] < 1 {
		t.Fatal("JSON masking not executed or reported")
	}
	if got := labOracleJSONProtected(result.Output, labOracleCanary); got != "" {
		t.Fatalf("protected JSON: %s", got)
	}
	back, err := lab.Restore(context.Background(), result.ID, result.Output)
	if err != nil || back.Output != labOracleJSON {
		t.Fatal("own JSON restore differs from independently specified input", err)
	}
}

func TestLabOracleRestoreRejection(t *testing.T) {
	lab := NewLab(t.TempDir())
	first := labOraclePreview(t, lab, PreviewInput{Mode: "text", Input: "password=RR-LAB-SYN-7319 keep-sentinel", Rules: json.RawMessage(`{}`), Enabled: true})
	second := labOraclePreview(t, lab, PreviewInput{Mode: "text", Input: "password=RR-LAB-OTHER-8821 keep-sentinel", Rules: json.RawMessage(`{}`), Enabled: true})
	if first.ID == second.ID {
		t.Fatal("independent lab capabilities unexpectedly alias")
	}
	const foreignReal = "RR-LAB-OTHER-8821"
	if reason := labOracleForeignRestore(PreviewResult{Output: foreignReal}, context.Canceled, foreignReal); reason != "foreign-real" {
		t.Fatalf("error plus foreign partial output escaped observer: %s", reason)
	}
	if reason := labOracleForeignRestore(PreviewResult{Output: "keep-sentinel"}, context.Canceled, foreignReal); reason != "partial-output" {
		t.Fatalf("error plus other partial output escaped observer: %s", reason)
	}
	foreign, err := lab.Restore(context.Background(), first.ID, second.Output)
	if reason := labOracleForeignRestore(foreign, err, foreignReal); reason != "" {
		t.Fatalf("foreign handle restore: %s", reason)
	}
	for _, tc := range []struct {
		id, output, own string
	}{
		{first.ID, first.Output, "password=RR-LAB-SYN-7319 keep-sentinel"},
		{second.ID, second.Output, "password=RR-LAB-OTHER-8821 keep-sentinel"},
	} {
		got, restoreErr := lab.Restore(context.Background(), tc.id, tc.output)
		if restoreErr != nil || got.Output != tc.own {
			t.Fatal("own handle failed after foreign attempt", restoreErr)
		}
	}
	lab.Clear(first.ID)
	if got, restoreErr := lab.Restore(context.Background(), first.ID, first.Output); restoreErr == nil || got.Output != "" {
		t.Fatal("cleared handle returned output")
	}

	jsonResult := labOraclePreview(t, lab, PreviewInput{Mode: "json", Input: labOracleJSON, Rules: json.RawMessage(labOracleRules), Enabled: true})
	if reason := labOracleJSONProtected(jsonResult.Output, labOracleCanary); reason != "" {
		t.Fatalf("protected JSON structure before refusal control: %s", reason)
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(jsonResult.Output), &root); err != nil {
		t.Fatal("lab returned invalid protected JSON")
	}
	blocks := root["messages"].([]any)[0].(map[string]any)["content"].([]any)
	input := blocks[1].(map[string]any)["input"].(map[string]any)
	input["password"] = "<secret:damaged>"
	bad, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, restoreErr := lab.Restore(context.Background(), jsonResult.ID, string(bad)); restoreErr == nil || got.Output != "" {
		t.Fatal("damaged typed placeholder returned partial restoration")
	}
	good, err := lab.Restore(context.Background(), jsonResult.ID, jsonResult.Output)
	if err != nil || good.Output != labOracleJSON {
		t.Fatal("valid restore failed after rejected typed value", err)
	}
}

func TestLabOracleInputRejection(t *testing.T) {
	lab := NewLab(t.TempDir())
	base := PreviewInput{Mode: "text", Input: "password=RR-LAB-SYN-7319 keep-sentinel", Rules: json.RawMessage(`{}`), Enabled: true}
	cases := []struct {
		name   string
		change func(*PreviewInput)
	}{
		{"mode", func(in *PreviewInput) { in.Mode = "binary" }},
		{"utf8", func(in *PreviewInput) { in.Input = string([]byte{0xff}) }},
		{"json", func(in *PreviewInput) { in.Mode, in.Input = "json", `{"system":` }},
		{"rules", func(in *PreviewInput) { in.Rules = json.RawMessage(`{"filters":{"secret":null}}`) }},
		{"filter", func(in *PreviewInput) { in.Filter = "not-a-filter" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.change(&in)
			got, err := lab.Preview(context.Background(), in)
			if err == nil || got.ID != "" || got.Output != "" || lab.State().Active != 0 {
				t.Fatal("invalid input produced output or active handle")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := lab.Preview(ctx, base)
	if err == nil || got.ID != "" || got.Output != "" || lab.State().Active != 0 {
		t.Fatal("cancelled preview produced output or active handle")
	}
	valid := labOraclePreview(t, lab, base)
	if reason := labOracleText(valid.Output, labOracleCanary, "keep-sentinel"); reason != "" {
		t.Fatalf("valid neighboring preview: %s", reason)
	}
	state, err := json.Marshal(lab.State())
	if err != nil || strings.Contains(string(state), labOracleCanary) || strings.Contains(string(state), valid.ID) {
		t.Fatal("rejection contaminated aggregate state", err)
	}
}

func TestLabOracleSupportedFields(t *testing.T) {
	const source = `{"system":"keep-sentinel 10.24.8.12 ops@example.internal host.example.internal","model":"claude-synthetic","messages":[{"role":"user","content":[{"type":"text","text":"Привет \"мир\"\nkeep-sentinel"},{"type":"tool_use","id":"tool_1","name":"connect","input":{"password":"RR-LAB-SYN-7319","port":22}}]}],"metadata":{"user_id":"rr-lab-test","billing":"credits-7"}}`
	rules := json.RawMessage(`{"domains":["example.internal"],"entries":[{"kind":"host","forms":["host.example.internal"]}],"fields":[{"path":"messages[*].content[*].input.password","kind":"secret"}]}`)
	lab := NewLab(t.TempDir())
	for _, canary := range []string{labOracleCanary, "10.24.8.12", "ops@example.internal", "host.example.internal"} {
		if got := labOracleJSONProtected(source, canary); got != "leak" {
			t.Fatalf("source positive control: %s", got)
		}
	}
	result := labOraclePreview(t, lab, PreviewInput{Mode: "json", Input: source, Rules: rules, Enabled: true})
	if !result.Roundtrip || result.Masked[KindIPv4] < 1 || result.Masked[KindEmail] < 1 || result.Masked[KindHost] < 1 || result.Masked[KindSecret] < 1 {
		t.Fatal("supported field classes were not all transformed")
	}
	if got := labOracleJSONProtected(result.Output, labOracleCanary, "10.24.8.12", "ops@example.internal", "host.example.internal"); got != "" {
		t.Fatalf("protected supported fields: %s", got)
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(result.Output), &root); err != nil {
		t.Fatal(err)
	}
	system := root["system"].(string)
	parts := strings.Fields(system)
	if len(parts) != 4 || parts[0] != "keep-sentinel" {
		t.Fatal("supported fields changed safe text or count")
	}
	ip, err := netip.ParseAddr(parts[1])
	if err != nil || !netip.MustParsePrefix("100.64.0.0/10").Contains(ip) || !strings.Contains(parts[2], "@") || parts[3] == "" {
		t.Fatal("pseudonym outside stated IPv4 range or malformed supported field")
	}
	for _, output := range []string{
		strings.Replace(result.Output, `"model":"claude-synthetic"`, `"model":"changed"`, 1),
		strings.Replace(result.Output, `"name":"connect"`, `"name":"changed"`, 1),
		strings.Replace(result.Output, `"billing":"credits-7"`, `"billing":"changed"`, 1),
		strings.Replace(result.Output, `,"port":22`, "", 1),
		strings.Replace(result.Output, parts[1], "203.0.113.7", 1),
		strings.Replace(result.Output, parts[1], "10.24.8.12", 1),
		strings.Replace(result.Output, parts[1], string([]byte{92, 117, 48, 48, 51, 49, 48})+".24.8.12", 1),
	} {
		if output == result.Output || labOracleJSONProtected(output, labOracleCanary, "10.24.8.12", "ops@example.internal", "host.example.internal") == "" {
			t.Fatal("corrupted supported-field output passed unchanged observer")
		}
	}
	blocks := root["messages"].([]any)[0].(map[string]any)["content"].([]any)
	root["messages"].([]any)[0].(map[string]any)["content"] = blocks[:1]
	missingTool, err := json.Marshal(root)
	if err != nil || labOracleJSONProtected(string(missingTool), labOracleCanary, "10.24.8.12", "ops@example.internal", "host.example.internal") != "tool-structure" {
		t.Fatal("missing tool escaped unchanged observer", err)
	}
	back, err := lab.Restore(context.Background(), result.ID, result.Output)
	if err != nil || back.Output != source {
		t.Fatal("own supported-field restore differs from input", err)
	}
}
