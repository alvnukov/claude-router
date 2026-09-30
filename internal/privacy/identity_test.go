package privacy

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCaseSensitiveIdentityAcrossTurns(t *testing.T) {
	r, err := ParseRules([]byte(`{"entries":[{"kind":"person","forms":["Ab.cdefghiJ"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, r)
	masked, _ := mustMask(t, e, requestBody("case", "Ab.cdefghiJ"))
	pseudo, _ := lookupString(masked, "system")
	_, next := mustMask(t, e, requestBody("case", "next turn"))
	p, _ := json.Marshal(pseudo)
	reply := []byte(`{"content":[{"type":"tool_use","name":"login","input":{"username":` + string(p) + `}}]}`)
	out, err := e.UnmaskJSON(next, reply)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"username":"Ab.cdefghiJ"`)) {
		t.Fatalf("case-sensitive identity changed: %s", out)
	}
}
func TestSameTextDifferentKinds(t *testing.T) {
	sd := &sessionData{entries: make(map[string]mapRecord)}
	sd.add(mapRecord{Kind: KindHost, Real: "admin", Pseudo: "host-fictitious"})
	sd.add(mapRecord{Kind: KindPerson, Real: "admin", Pseudo: "person-fictitious"})
	if len(sd.entries) != 2 {
		t.Fatal("unrelated entity kinds were merged")
	}
}
func TestSplitIPv6(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	masked, req := mustMask(t, e, requestBody("ipv6", "fd12:3456:789a::2"))
	pseudo, _ := lookupString(masked, "system")
	var sink bytes.Buffer
	w := e.NewStreamUnmasker(req, &sink)
	for _, r := range pseudo {
		if _, err := w.Write([]byte(streamDelta(0, "text_delta", string(r)))); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := streamText(t, sink.String()); got != "fd12:3456:789a::2" {
		t.Fatalf("split IPv6 restored incorrectly: %q", got)
	}
}

func TestFieldKindsAndExactIdentity(t *testing.T) {
	r, err := ParseRules([]byte(`{"fields":[{"path":"messages[*].content[*].input.username","kind":"login"},{"path":"messages[*].content[*].input.server","kind":"host"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, r)
	body := []byte(`{"messages":[{"content":[{"type":"tool_use","input":{"username":"Admin","server":"Admin"}}]}]}`)
	masked, req := mustMask(t, e, body)
	login, _ := lookupString(masked, "messages", "0", "content", "0", "input", "username")
	server, _ := lookupString(masked, "messages", "0", "content", "0", "input", "server")
	if login == server || login == "Admin" || server == "Admin" {
		t.Fatal("field kinds lost")
	}
	restored, err := e.UnmaskJSON(req, masked)
	if err != nil || !bytes.Equal(restored, body) {
		t.Fatal("identity mismatch", err)
	}
}

func TestAliasSubstringNotRewritten(t *testing.T) {
	e := testEngine(t, corpusRules(t))
	masked, req := mustMask(t, e, requestBody("substring", "Ромашка"))
	pseudo, _ := lookupString(masked, "system")
	for _, value := range []string{"prefix" + pseudo, pseudo + "suffix"} {
		b, _ := json.Marshal(map[string]string{"system": value})
		out, err := e.UnmaskJSON(req, b)
		if err != nil || !bytes.Equal(out, b) {
			t.Fatal("unrelated word rewritten", err)
		}
	}
}

func TestAllowedNetworkCollisionRejected(t *testing.T) {
	r := corpusRules(t)
	m, err := newIPMapper(subkey(testIPKey, "ip"), r, prfV2)
	if err != nil {
		t.Fatal(err)
	}
	const real = "10.1.2.3"
	r.Allow = append(r.Allow, mapNetwork(m, real, false))
	e := testEngine(t, r)
	e.store.newKey = func() ([]byte, error) { return bytes.Clone(testIPKey), nil }
	out, req, err := e.Mask(requestBody("allow-collision", real))
	if err == nil || out != nil || req != nil {
		t.Fatal("ambiguous allowed alias was issued")
	}
}
