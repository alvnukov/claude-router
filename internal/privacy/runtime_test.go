package privacy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runtimeConfig(t *testing.T, home, rules string) {
	t.Helper()
	body := `{"version":1,"enabled":true,"default":"safe","profiles":[{"id":"safe","name":"Safe","enabled":true,"rules":` + rules + `}],"bindings":[]}`
	if err := os.WriteFile(filepath.Join(home, "privacy-profiles.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestRuntimeSnapshotAndSessionIsolation(t *testing.T) {
	home := t.TempDir()
	runtimeConfig(t, home, `{}`)
	rt := NewRuntime(home)
	policy, err := rt.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	body := requestBody("same-session", "10.2.3.4 password=Canary-password-123")
	a, masked, err := policy.Prepare(Target{}, body)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if bytes.Contains(masked, []byte("10.2.3.4")) || bytes.Contains(masked, []byte("Canary")) || !bytes.Contains(masked, []byte("metadata")) {
		t.Fatalf("wire leak: %s", masked)
	}
	b, again, err := policy.Prepare(Target{}, body)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if !bytes.Equal(masked, again) {
		t.Fatal("session aliases changed")
	}
	var in map[string]json.RawMessage
	_ = json.Unmarshal(masked, &in)
	restored, err := a.Restore(append(append([]byte(`{"content":`), in["system"]...), '}'), false)
	if err != nil || !bytes.Contains(restored, []byte("Canary-password-123")) {
		t.Fatal("restoration failed", err)
	}
	runtimeConfig(t, home, `{"filters":{"phone":false}}`)
	next, err := rt.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	c, changed, err := next.Prepare(Target{}, body)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if bytes.Equal(masked, changed) {
		t.Fatal("new rules reused old dictionary")
	}
	// In-flight old snapshot remains valid after reload.
	d, old, err := policy.Prepare(Target{}, body)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if !bytes.Equal(masked, old) {
		t.Fatal("snapshot mutated")
	}
}
func TestRuntimeInvalidAndMissingNeverDisable(t *testing.T) {
	home := t.TempDir()
	rt := NewRuntime(home)
	p, err := rt.Snapshot()
	if err != nil || p.Enabled() {
		t.Fatal("fresh config should be off")
	}
	runtimeConfig(t, home, `{}`)
	if _, err = rt.Snapshot(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "privacy-profiles.json")
	if err = os.WriteFile(path, []byte(`{"enabled":false,"broken":`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = rt.Snapshot(); err == nil {
		t.Fatal("invalid snapshot bypass")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for _, runtime := range []*Runtime{rt, NewRuntime(home)} {
		if _, err = runtime.Snapshot(); err == nil {
			t.Fatal("deletion bypass")
		}
	}
}
func TestTransportPreservesUncoveredValues(t *testing.T) {
	home := t.TempDir()
	runtimeConfig(t, home, `{"entries":[{"kind":"org","forms":["PrivateCanary"]}]}`)
	p, err := NewRuntime(home).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"system":"ok","unknown":"safe"}`,
		`{"system":"ok","tools":[{"name":"PrivateCanary","input_schema":{"type":"object"}}]}`,
		`{"system":"ok","tools":[{"name":"safe","input_schema":{"type":"object","pattern":"PrivateCanary"}}]}`,
		`{"system":"ok","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"safe","signature":"opaque"}]}]}`,
		`{"system":"ok","stop_sequences":["10.2.3.4"]}`,
		`{"system":"x-anthropic-billing-header: PrivateCanary"}`,
	} {
		x, wire, err := p.Prepare(Target{}, []byte(body))
		if err != nil || x == nil {
			t.Fatalf("unsupported content blocked supported text: %v", err)
		}
		x.Close()
		if strings.Contains(body, "x-anthropic-billing-header") {
			if bytes.Contains(wire, []byte("PrivateCanary")) {
				t.Fatal("supported system text not masked")
			}
		} else if !bytes.Equal(wire, []byte(body)) {
			t.Fatalf("uncovered values changed: %s", wire)
		}
	}
}
func TestTransportSSEAtomicAndUnknownAliasPreserved(t *testing.T) {
	home := t.TempDir()
	runtimeConfig(t, home, `{}`)
	p, err := NewRuntime(home).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	x, masked, err := p.Prepare(Target{}, []byte(`{"system":"password=Canary-password-123"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	var obj map[string]string
	_ = json.Unmarshal(masked, &obj)
	pseudo := strings.TrimPrefix(obj["system"], "password=")
	delta, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": pseudo}})
	sse := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: " + string(delta) + "\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	out, err := x.Restore(sse, true)
	if err != nil || !bytes.Contains(out, []byte("Canary-password-123")) {
		t.Fatal("stream failed", err)
	}
	out, err = x.Restore(sse[:bytes.Index(sse, []byte("event: message_stop"))], true)
	if err == nil || len(out) != 0 {
		t.Fatal("partial stream accepted")
	}
	unknown := []byte(`{"content":"<secret:credential:deadbeef>"}`)
	out, err = x.Restore(unknown, false)
	if err != nil || !bytes.Equal(out, unknown) {
		t.Fatal("unknown alias changed or rejected", err)
	}

}

func TestTransportResponseIdentifiersAndOpaqueBlocks(t *testing.T) {
	home := t.TempDir()
	runtimeConfig(t, home, `{}`)
	p, err := NewRuntime(home).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	x, masked, err := p.Prepare(Target{}, []byte(`{"system":"password=Canary-password-123"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	var obj map[string]string
	_ = json.Unmarshal(masked, &obj)
	pseudo := strings.TrimPrefix(obj["system"], "password=")
	for _, bad := range []string{
		`{"content":[{"type":"thinking","thinking":"` + pseudo + `","signature":"opaque"}]}`,
		`{"content":[{"type":"tool_use","name":"` + pseudo + `","id":"1","input":{}}]}`,
		`{"content":[{"type":"text","text":"safe","extra":"` + pseudo + `"}]}`,
	} {
		out, err := x.Restore([]byte(bad), false)
		if err != nil || !bytes.Equal(out, []byte(bad)) {
			t.Error("opaque block, structural ID or unknown field changed", err)
		}
	}
}

func TestTransportShortSecretsAndInvalidUTF8(t *testing.T) {
	home := t.TempDir()
	runtimeConfig(t, home, `{}`)
	p, err := NewRuntime(home).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	x, masked, err := p.Prepare(Target{}, []byte(`{"system":"password=abc"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	if bytes.Contains(masked, []byte("abc")) {
		t.Fatal("short password leaked")
	}
	x, _, err = p.Prepare(Target{}, []byte("{\"system\":\"\xff\"}"))
	if x != nil {
		x.Close()
	}
	if err == nil {
		t.Fatal("invalid UTF8 silently replaced")
	}
}
