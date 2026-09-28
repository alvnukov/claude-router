package privacy

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func supportedContentEngine(t *testing.T) *Engine {
	t.Helper()
	rules, err := ParseRules([]byte(`{"entries":[{"kind":"org","forms":["PrivateOrg","OtherOrg"]}],"fields":[{"path":"messages[*].content[*].input.count","kind":"login"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e, err := Open(t.TempDir(), rules, Options{supportedOnly: true, Home: "/home/synthetic", Hostname: "synthetic.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSupportedContentMixedMaskKeepsOpaqueSpans(t *testing.T) {
	e := supportedContentEngine(t)
	body := []byte(`{"service":{"text":"PrivateOrg"},"system":"PrivateOrg","messages":[{"role":"assistant","content":[{"type":"future_block","text":"PrivateOrg","data":"\\u0410"},{"type":"thinking","thinking":"PrivateOrg","signature":"opaque\\u0041"},{"type":"image","source":{"type":"url","url":"https://PrivateOrg.invalid"}},{"type":"document","source":{"data":"PrivateOrg"}},{"type":"text","text":"PrivateOrg","future":"PrivateOrg"}]}],"tools":[{"name":"mcp__claude_ai_PrivateOrg","description":"PrivateOrg","input_schema":{"type":"object","x-service":{"description":"PrivateOrg"},"properties":{"PrivateOrg":{"type":"string","description":"PrivateOrg"}}}}]}`)
	masked, req, err := e.Mask(body)
	if err != nil {
		t.Fatal(err)
	}
	defer req.Close()
	for _, exact := range []string{`"service":{"text":"PrivateOrg"}`, `{"type":"future_block","text":"PrivateOrg","data":"\\u0410"}`, `{"type":"thinking","thinking":"PrivateOrg","signature":"opaque\\u0041"}`, `{"type":"image","source":{"type":"url","url":"https://PrivateOrg.invalid"}}`, `{"type":"document","source":{"data":"PrivateOrg"}}`, `"future":"PrivateOrg"`, `"name":"mcp__claude_ai_PrivateOrg"`, `"properties":{"PrivateOrg":`, `"x-service":{"description":"PrivateOrg"}`} {
		if !bytes.Contains(masked, []byte(exact)) {
			t.Fatalf("opaque/structural span changed: %s", exact)
		}
	}
	if bytes.Contains(masked, []byte(`"system":"PrivateOrg"`)) || bytes.Contains(masked, []byte(`"name":"mcp__claude_ai_PrivateOrg","description":"PrivateOrg"`)) || bytes.Contains(masked, []byte(`"type":"text","text":"PrivateOrg"`)) {
		t.Fatal("supported text remained unmasked")
	}
	restored, err := e.UnmaskJSON(req, masked)
	if err != nil || !bytes.Equal(restored, body) {
		t.Fatalf("round trip did not preserve mixed document: %v\n%s", err, restored)
	}
}

func TestSupportedContentPreservesTypedValuesAndKeys(t *testing.T) {
	e := supportedContentEngine(t)
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"PrivateOrg","name":"PrivateOrg","input":{"PrivateOrg":"PrivateOrg","12345":"PrivateOrg","count":12345,"flag":true,"empty":null}}]}]}`)
	masked, req, err := e.Mask(body)
	if err != nil {
		t.Fatal(err)
	}
	defer req.Close()
	for _, exact := range []string{`"id":"PrivateOrg"`, `"name":"PrivateOrg"`, `"PrivateOrg":`, `"12345":`, `"count":12345`, `"flag":true`, `"empty":null`} {
		if !bytes.Contains(masked, []byte(exact)) {
			t.Fatalf("structural span changed: %s", exact)
		}
	}
	if bytes.Contains(masked, []byte(`"PrivateOrg":"PrivateOrg"`)) {
		t.Fatal("string tool input was not masked")
	}
	restored, err := e.UnmaskJSON(req, masked)
	if err != nil || !bytes.Equal(restored, body) {
		t.Fatalf("typed tool round trip failed: %v %s", err, restored)
	}
}

func TestSupportedContentHistoricalThinkingWithoutDictionary(t *testing.T) {
	e := supportedContentEngine(t)
	for _, metadata := range []string{"", `"metadata":{"user_id":"{\"session_id\":\"never-seen\"}"},`} {
		body := []byte(`{` + metadata + `"system":"PrivateOrg","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"PrivateOrg","signature":"opaque-signature"}]}]}`)
		masked, req, err := e.Mask(body)
		if err != nil {
			t.Fatal(err)
		}
		req.Close()
		if !bytes.Contains(masked, []byte(`{"type":"thinking","thinking":"PrivateOrg","signature":"opaque-signature"}`)) || bytes.Contains(masked, []byte(`"system":"PrivateOrg"`)) {
			t.Fatal("historical thinking or known text mishandled")
		}
	}
}

func TestSupportedContentRestoreOwnAndForeignAliases(t *testing.T) {
	e := supportedContentEngine(t)
	foreign, foreignReq, err := e.Mask(requestBody("foreign", "OtherOrg"))
	if err != nil {
		t.Fatal(err)
	}
	defer foreignReq.Close()
	own, ownReq, err := e.Mask(requestBody("own", "PrivateOrg"))
	if err != nil {
		t.Fatal(err)
	}
	defer ownReq.Close()
	foreignAlias, _ := lookupString(foreign, "system")
	ownAlias, _ := lookupString(own, "system")
	body := []byte(`{"content":[{"type":"tool_use","input":{"own":"` + ownAlias + `","foreign":"` + foreignAlias + `","unknown":"<secret:password:01234567>","network":"203.0.113.77"}},{"type":"future_block","text":"` + ownAlias + `"}]}`)
	restored, err := e.UnmaskJSON(ownReq, body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(restored, []byte(`"own":"PrivateOrg"`)) || bytes.Contains(restored, []byte("OtherOrg")) {
		t.Fatalf("wrong owner restored: %s", restored)
	}
	for _, exact := range []string{`"foreign":"` + foreignAlias + `"`, `"unknown":"<secret:password:01234567>"`, `"network":"203.0.113.77"`, `{"type":"future_block","text":"` + ownAlias + `"}`} {
		if !bytes.Contains(restored, []byte(exact)) {
			t.Fatalf("unknown span changed: %s", exact)
		}
	}
}

func TestSupportedContentDetectSkipsOpaqueAndTypedData(t *testing.T) {
	e := supportedContentEngine(t)
	body := []byte(`{"messages":[{"content":[{"type":"future","text":"OtherOrg"},{"type":"thinking","thinking":"OtherOrg"},{"type":"text","text":"PrivateOrg"},{"type":"tool_use","input":{"OtherOrg":12,"count":12345}}]}]}`)
	counts, err := e.Detect(body)
	if err != nil || counts[KindOrg] != 1 || counts[KindLogin] != 0 {
		t.Fatalf("unsupported data affected detection: %v %v", counts, err)
	}
	for _, body := range []string{`{"system":"a","system":"b"}`, "{\"system\":\"" + string([]byte{255}) + "\"}"} {
		if _, req, err := e.Mask([]byte(body)); err == nil {
			if req != nil {
				req.Close()
			}
			t.Fatal("invalid JSON admitted")
		}
	}
	if _, err := e.Detect([]byte(`{"system":"` + strings.Repeat("a", TrafficInputLimit) + `"}`)); err == nil {
		t.Fatal("oversize input admitted")
	}
}

func TestSupportedContentRetainsCryptoAndRestoreLimits(t *testing.T) {
	t.Run("key failure", func(t *testing.T) {
		e := supportedContentEngine(t)
		e.store.newKey = func() ([]byte, error) { return nil, errors.New("synthetic key failure") }
		wire, req, err := e.Mask([]byte(`{"system":"PrivateOrg","content":[{"type":"thinking","thinking":"opaque"}]}`))
		if err == nil || wire != nil || req != nil {
			t.Fatal("key failure became raw passthrough")
		}
	})
	t.Run("restoration budget", func(t *testing.T) {
		e := supportedContentEngine(t)
		wire, req, err := e.Mask([]byte(`{"system":"password=` + strings.Repeat("A", 128) + `"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer req.Close()
		req.budget = &restoreBudget{remaining: 1}
		if out, err := e.UnmaskJSON(req, wire); err == nil || out != nil {
			t.Fatal("restore budget became optional")
		}
		req.Close()
		if _, err := e.UnmaskJSON(req, []byte(`{"content":[]}`)); err == nil {
			t.Fatal("closed dictionary became usable")
		}
	})
}

func TestSupportedContentMasksBillingHeaderSystemText(t *testing.T) {
	e := supportedContentEngine(t)
	body := []byte(`{"system":"x-anthropic-billing-header: PrivateOrg\nPrivateOrg"}`)
	masked, req, err := e.Mask(body)
	if err != nil {
		t.Fatal(err)
	}
	defer req.Close()
	if bytes.Contains(masked, []byte("PrivateOrg")) {
		t.Fatalf("known system text escaped masking: %s", masked)
	}
	if !bytes.Contains(masked, []byte("x-anthropic-billing-header: ")) {
		t.Fatal("unmatched system text changed")
	}
	restored, err := e.UnmaskJSON(req, masked)
	if err != nil || !bytes.Equal(restored, body) {
		t.Fatalf("system billing header round trip failed: %v %s", err, restored)
	}
}

func TestSupportedContentMasksDependentSchemaDescriptions(t *testing.T) {
	e := supportedContentEngine(t)
	body := []byte(`{"tools":[{"name":"test","input_schema":{"type":"object","dependentSchemas":{"PrivateOrg":{"description":"PrivateOrg","properties":{"OtherOrg":{"type":"string","description":"OtherOrg"}}}}}}]}`)
	masked, req, err := e.Mask(body)
	if err != nil {
		t.Fatal(err)
	}
	defer req.Close()
	if bytes.Contains(masked, []byte(`"description":"PrivateOrg"`)) || bytes.Contains(masked, []byte(`"description":"OtherOrg"`)) {
		t.Fatalf("known dependent schema text remained unmasked: %s", masked)
	}
	if !bytes.Contains(masked, []byte(`"dependentSchemas":{"PrivateOrg":`)) || !bytes.Contains(masked, []byte(`"properties":{"OtherOrg":`)) {
		t.Fatal("structural schema property names changed")
	}
	restored, err := e.UnmaskJSON(req, masked)
	if err != nil || !bytes.Equal(restored, body) {
		t.Fatalf("dependent schema round trip failed: %v %s", err, restored)
	}
}
