package privacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	sessionOraclePerson = "Аврора Тестова"
	sessionOracleOther  = "Борис Синтетик"
	sessionOracleIP     = "10.24.8.12"
)

func sessionOracleEngine(t *testing.T, home string, rules *Rules) *Engine {
	t.Helper()
	e, err := Open(home, rules, Options{Home: "/home/rr-synthetic", Hostname: "rr-host.invalid"})
	if err != nil {
		t.Fatalf("open synthetic session engine: %v", err)
	}
	return e
}

func sessionOracleRules(t *testing.T, data string) *Rules {
	t.Helper()
	rules, err := ParseRules([]byte(data))
	if err != nil {
		t.Fatalf("parse synthetic session rules: %v", err)
	}
	return rules
}

func sessionOracleMask(t *testing.T, e *Engine, session, source string) (string, *Request) {
	t.Helper()
	masked, req, err := e.Mask(requestBody(session, source))
	if err != nil {
		t.Fatalf("mask synthetic session: %v", err)
	}
	t.Cleanup(req.Close)
	if req.Stats().Scope != "session" {
		t.Fatal("named session used request scope")
	}
	value, ok := lookupString(masked, "system")
	if !ok {
		t.Fatal("masked request omitted system text")
	}
	return value, req
}

// A protected session value is nonempty, contains no real input, and retains
// the independently chosen harmless suffix. Do not derive a golden from Mask.
func sessionOracleProtected(value, real string) string {
	if value == "" {
		return "empty"
	}
	if strings.Contains(value, real) {
		return "raw-real"
	}
	if !strings.HasSuffix(value, " keep-sentinel") {
		return "missing-allowed-text"
	}
	if strings.TrimSuffix(value, " keep-sentinel") == "" {
		return "empty-pseudonym"
	}
	return ""
}

func sessionOracleUnlink(a, b string) string {
	if a == b {
		return "linkable"
	}
	return ""
}

func sessionOracleStable(a, again string) string {
	if a != again {
		return "unstable"
	}
	return ""
}

func sessionOracleResponse(value string) []byte {
	body, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": value}}})
	return body
}

func sessionOracleResponseText(t *testing.T, body []byte) string {
	t.Helper()
	var root struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(body, &root) != nil || len(root.Content) != 1 || root.Content[0].Type != "text" {
		t.Fatal("restored response structure lost")
	}
	return root.Content[0].Text
}

func TestSessionOracleAutomaticPseudonymsA(t *testing.T) {
	const rulesJSON = `{"entries":[{"kind":"person","forms":["Аврора Тестова"]}]}`
	home := t.TempDir()
	rules := sessionOracleRules(t, rulesJSON)
	a := sessionOracleEngine(t, home, rules)
	b := sessionOracleEngine(t, home, rules)

	for _, real := range []string{sessionOraclePerson, sessionOracleIP} {
		if got := sessionOracleProtected(real+" keep-sentinel", real); got != "raw-real" {
			t.Fatalf("source positive control: %s", got)
		}
	}
	personA, reqPersonA := sessionOracleMask(t, a, "rr-one", sessionOraclePerson+" keep-sentinel")
	ipA, reqIPA := sessionOracleMask(t, a, "rr-one", sessionOracleIP+" keep-sentinel")
	personB, reqPersonB := sessionOracleMask(t, b, "rr-two", sessionOraclePerson+" keep-sentinel")
	ipB, reqIPB := sessionOracleMask(t, b, "rr-two", sessionOracleIP+" keep-sentinel")
	for _, tc := range []struct{ value, real string }{
		{personA, sessionOraclePerson}, {personB, sessionOraclePerson},
		{ipA, sessionOracleIP}, {ipB, sessionOracleIP},
	} {
		if got := sessionOracleProtected(tc.value, tc.real); got != "" {
			t.Fatalf("protected session value: %s", got)
		}
	}
	if got := sessionOracleUnlink(personA, personB); got != "" {
		t.Fatalf("automatic person: %s", got)
	}
	if got := sessionOracleUnlink(ipA, ipB); got != "" {
		t.Fatalf("automatic IPv4: %s", got)
	}
	for _, value := range []string{personA, personB, ipA, ipB} {
		pseudo := strings.TrimSuffix(value, " keep-sentinel")
		for _, real := range []string{sessionOraclePerson, sessionOracleIP} {
			if pseudo == real {
				t.Fatal("pseudonym equals another real value")
			}
		}
	}
	for i, value := range []string{personA, personB, ipA, ipB} {
		for j, other := range []string{personA, personB, ipA, ipB} {
			if i != j && strings.TrimSuffix(value, " keep-sentinel") == strings.TrimSuffix(other, " keep-sentinel") {
				t.Fatal("two independently assigned pseudonyms intersect")
			}
		}
	}
	if got := sessionOracleUnlink(personA, personA); got != "linkable" {
		t.Fatal("person collision mutation went undetected")
	}
	if got := sessionOracleUnlink(ipA, ipA); got != "linkable" {
		t.Fatal("IPv4 collision mutation went undetected")
	}
	if got := sessionOracleProtected(sessionOraclePerson+" keep-sentinel", sessionOraclePerson); got != "raw-real" {
		t.Fatal("raw mutation went undetected")
	}
	if got := sessionOracleProtected("", sessionOraclePerson); got != "empty" {
		t.Fatal("empty mutation went undetected")
	}
	if got := sessionOracleProtected(strings.TrimSuffix(personA, " keep-sentinel"), sessionOraclePerson); got != "missing-allowed-text" {
		t.Fatal("lost safe text went undetected")
	}

	again := sessionOracleEngine(t, home, rules)
	personAgain, reqPersonAgain := sessionOracleMask(t, again, "rr-one", sessionOraclePerson+" keep-sentinel")
	ipAgain, reqIPAgain := sessionOracleMask(t, again, "rr-one", sessionOracleIP+" keep-sentinel")
	if sessionOracleStable(personA, personAgain) != "" || sessionOracleStable(ipA, ipAgain) != "" {
		t.Fatal("same-session person or IPv4 changed across engines")
	}
	if sessionOracleStable(personA, personAgain+"mutated") != "unstable" || sessionOracleStable(ipA, ipAgain+"mutated") != "unstable" {
		t.Fatal("stability mutation went undetected")
	}
	for _, tc := range []struct {
		e    *Engine
		req  *Request
		got  string
		real string
	}{
		{a, reqPersonA, personA, sessionOraclePerson}, {a, reqIPA, ipA, sessionOracleIP},
		{b, reqPersonB, personB, sessionOraclePerson}, {b, reqIPB, ipB, sessionOracleIP},
		{again, reqPersonAgain, personAgain, sessionOraclePerson}, {again, reqIPAgain, ipAgain, sessionOracleIP},
	} {
		out, err := tc.e.UnmaskJSON(tc.req, sessionOracleResponse(tc.got))
		if err != nil || sessionOracleResponseText(t, out) != tc.real+" keep-sentinel" {
			t.Fatal("own session request failed independent restore", err)
		}
	}
	for _, id := range []string{"rr-one", "rr-two"} {
		if info, err := os.Stat(filepath.Join(home, "privacy", "sessions", id+".jsonl")); err != nil || !info.Mode().IsRegular() {
			t.Fatal("expected synthetic session file missing", err)
		}
	}
	t.Run("RejectForgedAutoDuplicate", func(t *testing.T) {
		sessionOracleReplacePersonRecord(t, home, "rr-two", sessionOraclePerson, strings.TrimSuffix(personA, " keep-sentinel"))
		if _, err := Open(home, rules, Options{Home: "/home/rr-synthetic", Hostname: "rr-host.invalid"}); err == nil {
			t.Fatal("forged automatic pseudonym duplicate was accepted")
		}
	})
}

func sessionOracleReplacePersonRecord(t *testing.T, home, id, real, pseudo string) {
	t.Helper()
	path := filepath.Join(home, "privacy", "sessions", id+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read synthetic session record", err)
	}
	lines := bytes.Split(data, []byte{'\n'})
	replaced := false
	for i, line := range lines {
		var record mapRecord
		if json.Unmarshal(line, &record) != nil || record.Kind != KindPerson || record.Real != sessionOraclePerson {
			continue
		}
		record.Real, record.Pseudo = real, pseudo
		lines[i], err = json.Marshal(record)
		if err != nil {
			t.Fatal("encode synthetic session record", err)
		}
		replaced = true
		break
	}
	if !replaced {
		t.Fatal("synthetic person record missing")
	}
	if err := os.WriteFile(path, bytes.Join(lines, []byte{'\n'}), 0o600); err != nil {
		t.Fatal("write synthetic session record", err)
	}
}

// Foreign restoration (B) is checked separately from pseudonym unlinkability
// (A); free-text foreign aliases can stay unchanged, never become foreign real.
func sessionOracleNoForeign(body []byte, foreign string) string {
	if len(body) == 0 {
		return "empty"
	}
	if bytes.Contains(body, []byte(foreign)) {
		return "foreign-real"
	}
	var parsed any
	if json.Unmarshal(body, &parsed) != nil {
		return "invalid-json"
	}
	if labOracleContains(parsed, foreign) {
		return "foreign-real"
	}
	return ""
}

func sessionOracleForeignText(body []byte, foreign, alias string) string {
	if reason := sessionOracleNoForeign(body, foreign); reason != "" {
		return reason
	}
	var response struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(body, &response) != nil || len(response.Content) != 1 || response.Content[0].Type != "text" {
		return "missing-text-block"
	}
	if response.Content[0].Text != alias+" keep-sentinel" {
		return "changed-foreign-alias"
	}
	return ""
}

func sessionOracleTyped(value string) []byte {
	body, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "tool_use", "name": "owner", "input": map[string]any{"owner": value}}}})
	return body
}

func sessionOracleTypedOwner(t *testing.T, body []byte) string {
	t.Helper()
	var root struct {
		Content []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Input struct {
				Owner string `json:"owner"`
			} `json:"input"`
		} `json:"content"`
	}
	if json.Unmarshal(body, &root) != nil || len(root.Content) != 1 || root.Content[0].Type != "tool_use" || root.Content[0].Name != "owner" {
		t.Fatal("typed response structure lost")
	}
	return root.Content[0].Input.Owner
}

func TestSessionOracleForeignRestorationB(t *testing.T) {
	const rulesJSON = `{"entries":[{"kind":"person","forms":["Аврора Тестова"]},{"kind":"person","forms":["Борис Синтетик"]}],"fields":[{"path":"content[*].input.owner","kind":"person"}]}`
	home := t.TempDir()
	rules := sessionOracleRules(t, rulesJSON)
	a := sessionOracleEngine(t, home, rules)
	b := sessionOracleEngine(t, home, rules)
	personA, reqA := sessionOracleMask(t, a, "rr-owner-a", sessionOraclePerson+" keep-sentinel")
	personB, reqB := sessionOracleMask(t, b, "rr-owner-b", sessionOracleOther+" keep-sentinel")
	if sessionOracleProtected(personA, sessionOraclePerson) != "" || sessionOracleProtected(personB, sessionOracleOther) != "" {
		t.Fatal("session B fixtures were not protected")
	}
	aliasA := strings.TrimSuffix(personA, " keep-sentinel")
	aliasB := strings.TrimSuffix(personB, " keep-sentinel")
	if aliasA == aliasB {
		t.Fatal("different owners received identical pseudonyms")
	}
	if got := sessionOracleForeignText(sessionOracleResponse(sessionOracleOther+" keep-sentinel"), sessionOracleOther, aliasB); got != "foreign-real" {
		t.Fatal("raw foreign positive control went undetected")
	}
	if got := sessionOracleForeignText(sessionOracleResponse(aliasB+" keep-sentinel"), sessionOracleOther, aliasB); got != "" {
		t.Fatal("intact foreign alias rejected by text observer")
	}
	if got := sessionOracleForeignText([]byte(`{"content":[]}`), sessionOracleOther, aliasB); got != "missing-text-block" {
		t.Fatal("empty-content mutation escaped foreign text observer")
	}
	if got := sessionOracleForeignText(sessionOracleResponse("changed keep-sentinel"), sessionOracleOther, aliasB); got != "changed-foreign-alias" {
		t.Fatal("changed foreign alias mutation escaped observer")
	}
	for _, tc := range []struct {
		e       *Engine
		req     *Request
		foreign string
		alias   string
	}{
		{a, reqA, sessionOracleOther, aliasB},
		{b, reqB, sessionOraclePerson, aliasA},
	} {
		out, err := tc.e.UnmaskJSON(tc.req, sessionOracleTyped(tc.alias))
		var reject *RejectError
		if err == nil || out != nil || !errors.As(err, &reject) {
			t.Fatal("typed foreign owner did not fail closed with RejectError")
		}
		if got := sessionOracleNoForeign(out, tc.foreign); got != "empty" {
			t.Fatalf("rejected typed response contained foreign data: %s", got)
		}
		before := tc.req.Stats().Unexpected
		out, err = tc.e.UnmaskJSON(tc.req, sessionOracleResponse(tc.alias+" keep-sentinel"))
		reason := sessionOracleForeignText(out, tc.foreign, tc.alias)
		if reason == "foreign-real" {
			t.Fatal("foreign free-text response disclosed another owner's real value")
		}
		if err != nil || reason != "" {
			t.Fatalf("foreign free-text output not preserved: %s", reason)
		}
		if tc.req == reqB && tc.req.Stats().Unexpected <= before {
			t.Fatal("known foreign free-text alias was not flagged")
		}
	}
	if got := sessionOracleNoForeign(sessionOracleTyped(sessionOracleOther), sessionOracleOther); got != "foreign-real" {
		t.Fatal("false successful typed foreign restore escaped observer")
	}
	for _, tc := range []struct {
		e    *Engine
		req  *Request
		own  string
		real string
	}{
		{a, reqA, aliasA, sessionOraclePerson},
		{b, reqB, aliasB, sessionOracleOther},
	} {
		typed, err := tc.e.UnmaskJSON(tc.req, sessionOracleTyped(tc.own))
		if err != nil || sessionOracleTypedOwner(t, typed) != tc.real {
			t.Fatal("own typed owner did not restore after foreign rejection", err)
		}
		out, err := tc.e.UnmaskJSON(tc.req, sessionOracleResponse(tc.own+" keep-sentinel"))
		if err != nil || sessionOracleResponseText(t, out) != tc.real+" keep-sentinel" {
			t.Fatal("own free-text restoration broken by foreign attempt", err)
		}
	}
	// Neither initial session map may acquire the other owner's real name.
	for _, tc := range []struct{ id, foreign string }{
		{"rr-owner-a", sessionOracleOther}, {"rr-owner-b", sessionOraclePerson},
	} {
		stored, err := os.ReadFile(filepath.Join(home, "privacy", "sessions", tc.id+".jsonl"))
		if err != nil || bytes.Contains(stored, []byte(tc.foreign)) {
			t.Fatal("foreign owner entered session map", err)
		}
	}
	// A foreign pseudonym in a NEW request is legitimately reissued under B.
	remasked, remaskRequest := sessionOracleMask(t, b, "rr-owner-b", aliasA+" keep-sentinel")
	if got := sessionOracleProtected(remasked, sessionOraclePerson); got != "" || strings.TrimSuffix(remasked, " keep-sentinel") == aliasA {
		t.Fatal("foreign input was not remasked for its receiving session")
	}
	own, err := b.UnmaskJSON(remaskRequest, sessionOracleResponse(remasked))
	if err != nil || sessionOracleResponseText(t, own) != sessionOraclePerson+" keep-sentinel" {
		t.Fatal("authorized remasked foreign input failed own restore", err)
	}
}

func TestSessionOracleExplicitPseudonymException(t *testing.T) {
	const explicit = "Вымышленный Маркер"
	const rulesJSON = `{"entries":[{"kind":"person","forms":["Аврора Тестова"],"pseudonym":"Вымышленный Маркер"}],"fields":[{"path":"content[*].input.owner","kind":"person"}]}`
	home := t.TempDir()
	rules := sessionOracleRules(t, rulesJSON)
	a := sessionOracleEngine(t, home, rules)
	b := sessionOracleEngine(t, home, rules)
	first, reqA := sessionOracleMask(t, a, "rr-explicit-a", sessionOraclePerson+" keep-sentinel")
	if first != explicit+" keep-sentinel" {
		t.Error("configured pseudonym differs from independent explicit rule")
	}
	second, reqB := sessionOracleMask(t, b, "rr-explicit-b", sessionOraclePerson+" keep-sentinel")
	if second != first || second != explicit+" keep-sentinel" {
		t.Fatal("same configured pseudonym did not remain intentionally linkable")
	}
	reopened := sessionOracleEngine(t, home, rules)
	third, reqAgain := sessionOracleMask(t, reopened, "rr-explicit-b", sessionOraclePerson+" keep-sentinel")
	if third != second {
		t.Fatal("configured pseudonym changed after reopening and third request")
	}
	for _, tc := range []struct {
		e   *Engine
		req *Request
	}{
		{a, reqA}, {b, reqB}, {reopened, reqAgain},
	} {
		out, err := tc.e.UnmaskJSON(tc.req, sessionOracleResponse(explicit+" keep-sentinel"))
		if err != nil || sessionOracleResponseText(t, out) != sessionOraclePerson+" keep-sentinel" {
			t.Fatal("own explicit-entry restore failed", err)
		}
		typed, err := tc.e.UnmaskJSON(tc.req, sessionOracleTyped(explicit))
		if err != nil || sessionOracleTypedOwner(t, typed) != sessionOraclePerson {
			t.Fatal("own typed explicit-entry restore failed", err)
		}
	}
	if sessionOracleUnlink(first, second) != "linkable" {
		t.Fatal("explicit exception control failed")
	}
	// The exception must never be fed to A's automatic-person/IP assertion.
	if sessionOracleUnlink(first, second) == "" {
		t.Fatal("automatic oracle accidentally accepted an explicit collision")
	}
	for _, badRules := range []string{
		`{}`,
		`{"entries":[{"kind":"person","forms":["Борис Синтетик"],"pseudonym":"Вымышленный Маркер"}]}`,
	} {
		if _, err := Open(home, sessionOracleRules(t, badRules), Options{Home: "/home/rr-synthetic", Hostname: "rr-host.invalid"}); err == nil {
			t.Fatal("explicit duplicate survived incompatible rules")
		}
	}
	sessionOracleReplacePersonRecord(t, home, "rr-explicit-b", sessionOracleOther, explicit)
	if _, err := Open(home, rules, Options{Home: "/home/rr-synthetic", Hostname: "rr-host.invalid"}); err == nil {
		t.Fatal("mismatched real reused an explicit pseudonym")
	}
}

func TestSessionOracleExplicitStemCollisionOnOpen(t *testing.T) {
	const baseRules = `{"entries":[{"kind":"person","forms":["Rrzx*"],"pseudonym":"Xqzzp"}]}`
	home := t.TempDir()
	rules := sessionOracleRules(t, baseRules)
	e := sessionOracleEngine(t, home, rules)
	masked, req := sessionOracleMask(t, e, "rr-explicit-stem", "Rrzxzzx keep-sentinel")
	if masked != "Xqzzpzzx keep-sentinel" {
		t.Fatal("explicit stem did not expand to its configured alias")
	}
	if _, err := Open(home, rules, Options{Home: "/home/rr-synthetic", Hostname: "rr-host.invalid"}); err != nil {
		t.Fatal("original explicit stem rejected on reopen", err)
	}
	for _, alias := range []string{"Xqzzpzzx", "xqzzpzzx"} {
		other := `{"entries":[{"kind":"person","forms":["Rrzx*"],"pseudonym":"Xqzzp"},{"kind":"person","forms":["Rrbx"],"pseudonym":"` + alias + `"}]}`
		if _, err := Open(home, sessionOracleRules(t, other), Options{Home: "/home/rr-synthetic", Hostname: "rr-host.invalid"}); err == nil {
			t.Fatalf("expanded alias %q was reassigned without rejection on Open", alias)
		}
	}
	back, err := e.UnmaskJSON(req, sessionOracleResponse(masked))
	if err != nil || sessionOracleResponseText(t, back) != "Rrzxzzx keep-sentinel" {
		t.Fatal("original stem mapping could not restore after refused rule", err)
	}
}
