package privacy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func detectProfileJSON(mode string) []byte {
	return []byte(`{"version":1,"enabled":true,"default":"safe","profiles":[{"id":"safe","name":"Safe","enabled":true,"mode":"` + mode + `","rules":{}}],"bindings":[]}`)
}

func TestDetectProfileValidationAndRuntime(t *testing.T) {
	for _, mode := range []string{"mask", "detect", "unknown"} {
		_, err := ParseProfiles(detectProfileJSON(mode))
		if (err == nil) != (mode != "unknown") {
			t.Fatalf("mode=%s err=%v", mode, err)
		}
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "privacy-profiles.json"), detectProfileJSON("detect"), 0600); err != nil {
		t.Fatal(err)
	}
	rt := NewRuntime(home)
	policy, err := rt.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	body := requestBody("debug-session", "10.2.3.4 password=Debug-Only-Secret-45")
	x, wire, err := policy.Prepare(Target{}, body)
	if err != nil || x != nil || !bytes.Equal(wire, body) {
		t.Fatalf("detect altered input or allocated restoration: exchange=%v err=%v", x != nil, err)
	}
	state, _ := json.Marshal(rt.State())
	if !bytes.Contains(state, []byte(`"detected":1`)) || !bytes.Contains(state, []byte(`"secret":1`)) || !bytes.Contains(state, []byte(`"ipv4":1`)) || bytes.Contains(state, []byte("Debug-Only")) {
		t.Fatalf("bad aggregate state: %s", state)
	}
	if rt.State().Protected != 0 || rt.State().Restored != 0 || rt.State().Bypassed != 0 {
		t.Fatal("detection counted as protection/bypass/restoration")
	}
	if _, err := os.Stat(filepath.Join(home, "privacy-runtime")); !os.IsNotExist(err) {
		t.Fatalf("detect created/read persistent dictionaries: %v", err)
	}
	// Existing configuration remains a mask policy, and a frozen detect policy
	// must not change when a later request activates masking.
	runtimeConfig(t, home, `{}`)
	mask, err := rt.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	protected, masked, err := mask.Prepare(Target{}, body)
	if err != nil || protected == nil || bytes.Equal(masked, body) {
		t.Fatal("mask default changed", err)
	}
	protected.Close()
	x, wire, err = policy.Prepare(Target{}, body)
	if err != nil || x != nil || !bytes.Equal(wire, body) {
		t.Fatal("detect snapshot changed")
	}
}

func TestDetectLabNoAliasesHandlesOrStorage(t *testing.T) {
	lab := NewLab(t.TempDir())
	secret := "Debug-Only-Secret-45"
	text := "10.2.3.4 password=" + secret + " encoded=" + base64.StdEncoding.EncodeToString([]byte(secret))
	var in PreviewInput
	raw, _ := json.Marshal(map[string]any{"mode": "text", "operation": "detect", "input": text, "enabled": true, "rules": map[string]any{}})
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	result, err := lab.Preview(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != text || result.ID != "" || result.Roundtrip || len(result.Masked) != 0 || lab.State().Active != 0 {
		t.Fatal("detect mutated content or created a restoration handle")
	}
	b, _ := json.Marshal(result)
	if !bytes.Contains(b, []byte(`"operation":"detect"`)) || !bytes.Contains(b, []byte(`"detected":{"ipv4":1,"secret":2}`)) {
		t.Fatalf("wrong findings: %s", b)
	}
	state, _ := json.Marshal(lab.State())
	if !bytes.Contains(state, []byte(`"detects":1`)) || strings.Contains(string(state), secret) || bytes.Contains(state, []byte(base64.StdEncoding.EncodeToString([]byte(secret)))) {
		t.Fatalf("unsafe lab stats: %s", state)
	}
	if _, err := lab.Restore(context.Background(), result.ID, text); err == nil {
		t.Fatal("detection supplied a restoration capability")
	}
}

func TestDetectLabHonorsFiltersAndRejectsUnknownOperation(t *testing.T) {
	for _, tc := range []struct {
		operation, filter string
		reject            bool
		want              string
	}{
		{"detect", "ipv4", false, `"detected":{"ipv4":1}`},
		{"detect", "secret", false, `"detected":{"secret":1}`},
		{"typo", "", true, ""},
	} {
		lab := NewLab(t.TempDir())
		var in PreviewInput
		b, _ := json.Marshal(map[string]any{"mode": "text", "operation": tc.operation, "filter": tc.filter, "enabled": false, "input": "10.2.3.4 password=Debug-Secret-45", "rules": map[string]any{}})
		_ = json.Unmarshal(b, &in)
		result, err := lab.Preview(context.Background(), in)
		if tc.reject {
			if err == nil {
				t.Fatal("invalid operation accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out, _ := json.Marshal(result)
		if !bytes.Contains(out, []byte(tc.want)) {
			t.Fatalf("isolated filter incorrect: %s", out)
		}
	}
}

func TestDetectJSONFieldsSourcesAndExemptions(t *testing.T) {
	const body = `{
  "system":"Demo Person ops@example.internal 10.2.3.4 LeaveMe",
  "messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"connect","input":{"password":"ab"}}]},{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"opaque-image-data"}}]}]
}`
	for _, disabled := range []bool{false, true} {
		rules := `{"domains":["example.internal"],"entries":[{"kind":"person","forms":["Demo Person","LeaveMe"]}],"allow":["LeaveMe"],"fields":[{"path":"messages[*].content[*].input.password","kind":"secret"}]`
		if disabled {
			rules += `,"filters":{"ipv4":false,"sources":false}`
		}
		rules += `}`
		lab := NewLab(t.TempDir())
		result, err := lab.Preview(context.Background(), PreviewInput{Mode: "json", Operation: ModeDetect, Input: body, Enabled: true, Rules: json.RawMessage(rules)})
		if err != nil || result.Output != body {
			t.Fatal("JSON detection changed data", err)
		}
		want := map[Kind]int{KindPerson: 1, KindEmail: 1, KindSecret: 1}
		if !disabled {
			want[KindIPv4], want[KindSource] = 1, 1
		}
		gotJSON, _ := json.Marshal(result.Detected)
		wantJSON, _ := json.Marshal(want)
		if !bytes.Equal(gotJSON, wantJSON) {
			t.Fatalf("findings=%s want=%s", gotJSON, wantJSON)
		}
	}
}

func TestDetectUnknownOperationNeverEntersObservation(t *testing.T) {
	lab := NewLab(t.TempDir())
	const canary = "password=Invalid-Operation-Secret"
	_, err := lab.Preview(context.Background(), PreviewInput{Operation: FilterMode(canary), Mode: "text", Input: "safe", Rules: json.RawMessage(`{}`), Enabled: true})
	if err == nil {
		t.Fatal("unknown operation accepted")
	}
	state, _ := json.Marshal(lab.State())
	if bytes.Contains(state, []byte(canary)) || lab.State().Restores != 0 {
		t.Fatalf("untrusted operation retained or counted as restoration: %s", state)
	}
}
