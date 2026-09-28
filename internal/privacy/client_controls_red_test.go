package privacy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// Compiling bridge for the Stage A direct-shape test while the later
// approved fixture files still reference not-yet-integrated package seams.
func TestTransportClientControlsCompilingRED(t *testing.T) {
	rules, err := ParseRules([]byte(`{"entries":[{"kind":"org","forms":["SyntheticPrivateName"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := Open(t.TempDir(), rules, Options{Home: "/home/testlogin", Hostname: "test-host.local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.checkTransport(clientCompatFirstTurnForRED()); err != nil {
		t.Fatalf("closed first-turn shape rejected: %v", err)
	}
}

func TestClientControlsUnapprovedModelPreservesControls(t *testing.T) {
	home := t.TempDir()
	profiles := []byte(`{"version":1,"enabled":true,"default":"safe","profiles":[{"id":"safe","name":"Safe","enabled":true,"mode":"mask","rules":{"entries":[]}}],"bindings":[]}`)
	if err := os.WriteFile(filepath.Join(home, "privacy-profiles.json"), profiles, 0600); err != nil {
		t.Fatal(err)
	}
	policy, err := NewRuntime(home).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	x, wire, err := policy.Prepare(Target{Model: "synthetic-supported", Provider: "anthropic"}, clientCompatFirstTurnForRED())
	if x != nil {
		x.Close()
	}
	if err != nil || x == nil || !bytes.Contains(wire, []byte(`"budget_tokens":31999`)) {
		t.Fatalf("unapproved controls blocked: exchange=%t wire=%d err=%v", x != nil, len(wire), err)
	}
}

func TestProtectedLocalCountCompilingRED(t *testing.T) {
	var localCalls atomic.Int32
	deps := HTTPDeps{
		Runtime: clientCompatRuntime(t, "mask"),
		Resolve: func([]byte) (HTTPRoute, error) {
			return HTTPRoute{Mode: "openai", Model: compatModel, Local: func(w http.ResponseWriter, r *http.Request, body []byte, effort string) {
				if _, err := FromRequest(r).Prepare(Target{Model: compatModel, Provider: "openai", Translated: true, TokenCount: true}, body); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				localCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": len(body) / 4})
			}}, nil
		},
	}
	server := httptest.NewServer(NewProtectedHTTP(deps))
	defer server.Close()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages/count_tokens", bytes.NewReader([]byte(`{"model":"synthetic-supported","messages":[{"role":"user","content":"plain"}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK || localCalls.Load() != 1 {
		t.Fatalf("unverified protected local count: status=%d calls=%d", resp.StatusCode, localCalls.Load())
	}
}

func clientCompatFirstTurnForRED() []byte {
	return []byte(`{"model":"synthetic-supported","max_tokens":40000,"stream":true,"thinking":{"type":"enabled","budget_tokens":31999,"display":"omitted"},"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},"messages":[{"role":"user","content":"hello SyntheticPrivateName"}]}`)
}
