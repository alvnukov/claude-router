package privacy

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	compatModel  = "synthetic-supported"
	compatBeta   = "compat-synthetic-0000"
	compatCanary = "SyntheticPrivateName"
)

// Only the test snapshot knows this invented model and beta. The production
// compatibility table must remain empty until real model support is approved.
func clientCompatRuntime(t *testing.T, mode string) *Runtime {
	t.Helper()
	home := t.TempDir()
	config := `{"version":1,"enabled":true,"default":"safe","profiles":[{"id":"safe","name":"Safe","enabled":true,"mode":"` + mode + `","rules":{"entries":[{"kind":"org","forms":["` + compatCanary + `"]}]}}],"bindings":[]}`
	if mode == "bypass" {
		config = strings.Replace(config, `"enabled":true,"mode":"bypass"`, `"enabled":false,"mode":"mask"`, 1)
	}
	if err := os.WriteFile(filepath.Join(home, "privacy-profiles.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	rt := NewRuntime(home)
	// Proposed test-only snapshot seam; Stage B must choose the final immutable
	// model/beta/limits representation before enabling this positive fixture.
	rt.clientControls = newClientControlTable([]ClientControlSupport{{
		Model: compatModel, Betas: []string{compatBeta}, MaxTokens: 40000, MaxBudgetTokens: 39999,
	}})
	return rt
}

func clientCompatBody() []byte {
	return []byte(`{"model":"synthetic-supported","max_tokens":40000,"stream":true,"thinking":{"type":"enabled","budget_tokens":31999,"display":"omitted"},"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},"system":"` + compatCanary + `","messages":[{"role":"user","content":[{"type":"text","text":"hello ` + compatCanary + `"}]}],"tools":[{"name":"run_synthetic","input_schema":{"type":"object","properties":{"query":{"type":"string"}}}}],"metadata":{"user_id":"synthetic-session-only"}}`)
}

func clientCompatTarget() Target {
	return Target{Model: compatModel, Provider: "anthropic", Betas: []string{compatBeta}}
}

func TestTransportClientControls(t *testing.T) {
	rules, err := ParseRules([]byte(`{"entries":[{"kind":"org","forms":["SyntheticPrivateName"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := Open(t.TempDir(), rules, Options{Home: "/home/testlogin", Hostname: "test-host.local"})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("direct-shape", func(t *testing.T) {
		// First compiling RED on @45aae67 when run alone: checkTransport
		// rejects context_management and enabled thinking before any masking.
		if err := engine.checkTransport(clientCompatBody()); err != nil {
			t.Fatalf("closed first-turn shape rejected: %v", err)
		}
	})

	base := string(clientCompatBody())
	cases := []struct{ name, body string }{
		{"unknown-root", strings.Replace(base, `"stream":true`, `"stream":true,"unknown":"safe"`, 1)},
		{"extra-thinking-key", strings.Replace(base, `"display":"omitted"`, `"display":"omitted","extra":"safe"`, 1)},
		{"extra-edit", strings.Replace(base, `"keep":"all"`, `"keep":"all","extra":true`, 1)},
		{"wrong-budget", strings.Replace(base, `"budget_tokens":31999`, `"budget_tokens":"31999"`, 1)},
		{"zero-budget", strings.Replace(base, `"budget_tokens":31999`, `"budget_tokens":0`, 1)},
		{"overflow-budget", strings.Replace(base, `"budget_tokens":31999`, `"budget_tokens":9223372036854775808`, 1)},
		{"budget-at-max", strings.Replace(base, `"budget_tokens":31999`, `"budget_tokens":40000`, 1)},
		{"model-limit", strings.Replace(base, `"max_tokens":40000`, `"max_tokens":40001`, 1)},
		{"unknown-model", strings.Replace(base, compatModel, `synthetic-unknown`, 1)},
		{"missing-beta", base},
		{"wrong-beta", base},
		{"duplicate-key", strings.Replace(base, `"model":"synthetic-supported"`, `"model":"synthetic-supported","model":"synthetic-supported"`, 1)},
		{"sensitive-control-scalar", strings.Replace(base, `"keep":"all"`, `"keep":"`+compatCanary+`"`, 1)},
		{"historical-signed-thinking", strings.Replace(base, `{"role":"user","content":`, `{"role":"assistant","content":[{"type":"thinking","thinking":"synthetic","signature":"opaque-synthetic"}]},{"role":"user","content":`, 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := clientCompatRuntime(t, "mask").Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			target := clientCompatTarget()
			switch tc.name {
			case "missing-beta":
				target.Betas = nil
			case "wrong-beta":
				target.Betas = []string{"unapproved-synthetic"}
			}
			x, wire, err := p.Prepare(target, []byte(tc.body))
			if x != nil {
				x.Close()
			}
			if err == nil || x != nil || len(wire) != 0 {
				t.Fatalf("invalid %s reached transport: exchange=%t wire=%d", tc.name, x != nil, len(wire))
			}
		})
	}
}

func TestClientControlsPolicyRoutes(t *testing.T) {
	body := clientCompatBody()
	for _, mode := range []string{"mask", "detect", "bypass"} {
		t.Run(mode, func(t *testing.T) {
			p, err := clientCompatRuntime(t, mode).Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			x, wire, err := p.Prepare(clientCompatTarget(), body)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "mask" {
				if x == nil {
					t.Fatal("mask has no restoration exchange")
				}
				defer x.Close()
				if bytes.Contains(wire, []byte(compatCanary)) || bytes.Contains(wire, []byte(`"metadata"`)) {
					t.Fatal("masked request leaked content or metadata")
				}
				for _, unchanged := range []string{`"budget_tokens":31999`, `"display":"omitted"`, `"clear_thinking_20251015"`, `"keep":"all"`, `"run_synthetic"`, `"input_schema"`} {
					if !bytes.Contains(wire, []byte(unchanged)) {
						t.Fatalf("approved control/tool semantics dropped: %s", unchanged)
					}
				}
			} else if x != nil || !bytes.Equal(wire, body) {
				if x != nil {
					x.Close()
				}
				t.Fatal("detect/bypass unexpectedly became protected or altered input")
			}
			target := clientCompatTarget()
			target.Translated = true
			if x, wire, err := p.Prepare(target, body); err == nil || x != nil || len(wire) != 0 {
				if x != nil {
					x.Close()
				}
				t.Fatal("translated controls passed candidate guard")
			}
		})
	}
}

func TestClientControlsOpaqueResponseIsNotReleased(t *testing.T) {
	p, err := clientCompatRuntime(t, "mask").Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	x, _, err := p.Prepare(clientCompatTarget(), clientCompatBody())
	if err != nil || x == nil {
		t.Fatal("synthetic supported first turn not prepared", err)
	}
	defer x.Close()
	for _, response := range []string{
		`{"content":[{"type":"thinking","thinking":"synthetic","signature":"opaque-synthetic"}]}`,
		`{"content":[{"type":"redacted_thinking","data":"opaque-synthetic"}]}`,
	} {
		out, err := x.Restore([]byte(response), false)
		if err == nil || len(out) != 0 {
			t.Fatal("opaque or signed response released")
		}
	}
}
