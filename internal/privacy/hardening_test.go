package privacy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestDuplicateJSONRejected(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	for _, body := range []string{`{"system":"safe","system":"Ромашка"}`, `{"messages":[{"content":[{"type":"thinking","type":"text","text":"Ромашка"}]}]}`} {
		out, req, err := e.Mask([]byte(body))
		if err == nil || out != nil || req != nil {
			t.Fatal("ambiguous JSON accepted")
		}
	}
}
func TestStructuredSecretsAndSchema(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	cases := []string{
		`{"messages":[{"content":[{"type":"tool_use","input":{"password":"x'\\y\"z","nested":{"api_key":"FAKE-long-credential"}}}]}]}`,
		`{"tools":[{"name":"safe","input_schema":{"properties":{"description":{"description":"Ромашка","default":"10.1.2.3"}},"examples":[{"company":"Ромашка"}],"const":"Ромашка"}}]}`,
	}
	for _, raw := range cases {
		body := []byte(raw)
		masked, req := mustMask(t, e, body)
		if bytes.Equal(masked, body) || bytes.Contains(masked, []byte("Ромашка")) || bytes.Contains(masked, []byte("FAKE-long-credential")) || bytes.Contains(masked, []byte(`x'`)) {
			t.Fatalf("structured value leaked: %s", masked)
		}
		restored, err := e.UnmaskJSON(req, masked)
		if err != nil || !bytes.Equal(restored, body) {
			t.Fatalf("structured roundtrip failed: %s, %v", restored, err)
		}
	}
}
func TestToolCorrectionInsteadOfRepair(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	masked, req := mustMask(t, e, requestBody("correction", "password=FAKE-password-1234\nРомашка 10.1.2.3"))
	text, _ := lookupString(masked, "system")
	secret := strings.TrimPrefix(strings.Split(text, "\n")[0], "password=")
	// An address the session did not issue is not a pseudonym: it passes as
	// is, in tool input as well.
	b, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "tool_use", "input": map[string]string{"command": "ping 10.123.45.67"}}}})
	if out, err := e.UnmaskJSON(req, b); err != nil || !bytes.Equal(out, b) {
		t.Errorf("foreign address in tool input: %s, %v", out, err)
	}
	for _, invalid := range []string{"<secret:credential:00000000>", strings.ToUpper(secret), base64.StdEncoding.EncodeToString([]byte(secret))} {
		b, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "tool_use", "input": map[string]string{"command": invalid}}}})
		out, err := e.UnmaskJSON(req, b)
		if err == nil || out != nil {
			t.Errorf("unsafe invocation returned for %q", invalid)
		}
		if err != nil && strings.Contains(err.Error(), "FAKE-password") {
			t.Fatal("error exposed a secret")
		}
	}
}
func TestUnknownContentRejected(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	out, req, err := e.Mask([]byte(`{"messages":[{"content":[{"type":"future_private_block","data":"sensitive"}]}]}`))
	if err == nil || out != nil || req != nil {
		t.Fatal("unknown content passed through")
	}
}
