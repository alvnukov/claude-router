package privacy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestReviewRegressions(t *testing.T) {
	t.Run("unicode-fold", func(t *testing.T) {
		rules, err := ParseRules([]byte(`{"entries":[{"kind":"person","forms":["Kroot"],"pseudonym":"Zuqevaxorimo"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		e := testEngine(t, rules)
		body := requestBody("unicodefold", "Kroot")
		masked, req := mustMask(t, e, body)
		out, err := e.UnmaskJSON(req, masked)
		if err != nil || !bytes.Equal(out, body) {
			t.Fatal("Unicode fold not reversible", err)
		}
	})
	t.Run("reopen-explicit", func(t *testing.T) {
		rules, _ := ParseRules([]byte(`{"entries":[{"kind":"org","forms":["SyntheticCompany"],"pseudonym":"Zuqevaxorimo"}]}`))
		home := t.TempDir()
		opt := Options{Home: "/home/testlogin", Hostname: "test-host.local"}
		e, err := Open(home, rules, opt)
		if err != nil {
			t.Fatal(err)
		}
		masked, req := mustMask(t, e, requestBody("restart", "SyntheticCompany"))
		req.Close()
		next, err := Open(home, rules, opt)
		if err != nil {
			t.Fatal("same configuration cannot reopen", err)
		}
		again, _ := mustMask(t, next, requestBody("restart", "SyntheticCompany"))
		if !bytes.Equal(masked, again) {
			t.Fatal("alias changed after reopen")
		}
	})
	t.Run("safe-errors", func(t *testing.T) {
		r, _ := ParseRules([]byte(`{"entries":[{"kind":"org","forms":["SensitiveCompany"]}]}`))
		e := testEngine(t, r)
		for _, body := range []string{`{"tools":[{"name":"SensitiveCompany"}]}`, `{"tools":[{"input_schema":{"properties":{"SensitiveCompany":{"type":"string"}}}}]}`} {
			err := e.Check([]byte(body))
			if err == nil || strings.Contains(err.Error(), "SensitiveCompany") {
				t.Fatal("unsafe diagnostic", err)
			}
		}
	})
	t.Run("known-secret-containers", func(t *testing.T) {
		e := testEngine(t, corpusRules(t))
		const secret = "FAKE-only-sensitive-value-984623"
		mustMask(t, e, requestBody("containers", "password="+secret))
		for _, value := range []string{secret, base64.StdEncoding.EncodeToString([]byte("result=[" + secret + "]")), "token=" + base64.StdEncoding.EncodeToString([]byte(secret))} {
			out, _ := mustMask(t, e, requestBody("containers", value))
			if bytes.Contains(out, []byte(value)) {
				t.Fatal("known secret leaked in container")
			}
		}
	})
	t.Run("response-kind", func(t *testing.T) {
		rules, _ := ParseRules([]byte(`{"fields":[{"path":"messages[*].content[*].input.server","kind":"host"},{"path":"messages[*].content[*].input.username","kind":"login"}]}`))
		e := testEngine(t, rules)
		masked, req := mustMask(t, e, []byte(`{"messages":[{"content":[{"type":"tool_use","input":{"server":"admin","username":"admin"}}]}]}`))
		alias, _ := lookupString(masked, "messages", "0", "content", "0", "input", "username")
		value, _ := json.Marshal(alias)
		out, err := e.UnmaskJSON(req, []byte(`{"content":[{"type":"tool_use","input":{"server":`+string(value)+`}}]}`))
		if err == nil || out != nil {
			t.Fatal("login accepted as host")
		}
		var sink bytes.Buffer
		w := e.NewStreamUnmasker(req, &sink)
		if _, err := w.Write([]byte(streamDelta(0, "input_json_delta", `{"server":`+string(value)+`}`))); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err == nil {
			t.Fatal("stream login accepted as host")
		}
	})
	t.Run("malformed-secret-reference", func(t *testing.T) {
		e := testEngine(t, corpusRules(t))
		masked, req := mustMask(t, e, requestBody("malformed-secret", "password=FAKE-password-1234"))
		value, _ := lookupString(masked, "system")
		alias := strings.TrimPrefix(value, "password=")
		for _, changed := range []string{strings.TrimSuffix(alias, ">"), strings.TrimPrefix(alias, "<"), strings.Replace(alias, "secret:", "secret/", 1)} {
			v, _ := json.Marshal(changed)
			out, err := e.UnmaskJSON(req, []byte(`{"content":[{"type":"tool_use","input":{"password":`+string(v)+`}}]}`))
			if err == nil || out != nil {
				t.Fatal("damaged secret reference accepted")
			}
		}
	})
}

func TestAllowedTypedValueRoundTrip(t *testing.T) {
	r, err := ParseRules([]byte(`{"allow":["PUBLIC_TEST_PASSWORD"]}`))
	if err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, r)
	body := []byte(`{"messages":[{"content":[{"type":"tool_use","input":{"password":"PUBLIC_TEST_PASSWORD"}}]}]}`)
	masked, req := mustMask(t, e, body)
	out, err := e.UnmaskJSON(req, masked)
	if err != nil || !bytes.Equal(out, body) {
		t.Fatal("allow not symmetric", err)
	}
}
