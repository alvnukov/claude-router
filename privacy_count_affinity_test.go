package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"localrouter/internal/privacy"
)

func TestPrivacyCountReusesAffinityWithoutChangingIt(t *testing.T) {
	oldTransport := http.DefaultTransport
	http.DefaultTransport = usageTransport(func(*http.Request) (*http.Response, error) {
		t.Error("token count attempted provider inference")
		return nil, errors.New("unexpected provider request")
	})
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	for _, state := range []string{"active", "expired", "changed", "removed", "other-scope", "missing"} {
		t.Run(state, func(t *testing.T) {
			home := t.TempDir()
			raw := `{"version":1,"enabled":true,"default":"mask","profiles":[{"id":"mask","name":"Mask","enabled":true,"mode":"mask","rules":{}},{"id":"detect","name":"Detect","enabled":true,"mode":"detect","rules":{}}],"bindings":[{"kind":"model","target":"p/other","profile":"detect"}]}`
			if err := os.WriteFile(filepath.Join(home, "privacy-profiles.json"), []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			rt := privacy.NewRuntime(home)
			policy, err := rt.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			base := config{Failover: true, Local: localSetup{Providers: []provider{{Name: "p", BaseURL: "http://not-contacted.invalid"}}, Models: []localModel{{Provider: "p", Model: "chosen"}, {Provider: "p", Model: "other"}}, Routes: map[string]map[string]modelRoute{"test": {"default": {Mode: "pool", Pool: "work"}}}, ModelPools: map[string][]poolTarget{"work": {{Model: "p/chosen"}, {Model: "p/other"}}}}}
			cfg := base.ForModel("test", "default")
			hl := newHealth("")
			cands := hl.pick(cfg)
			body := []byte(trafficBody)
			var decoded anthropicRequest
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatal(err)
			}
			scope := affinityKey(cfg, body, decoded)
			if scope == "" {
				t.Fatal("missing session affinity fixture")
			}
			if state != "missing" {
				hl.bindCandidates(scope, poolRoute{cfg.PoolName, cfg.PoolType}, cands)
				hl.moveSession(scope, cands[0].Key, cands[1])
				binding := hl.sessions[scope]
				if state == "expired" {
					binding.Used = time.Now().Add(-25 * time.Hour)
				}
				if state == "changed" {
					binding.Selection = "outdated-model-configuration"
				}
				if state == "removed" {
					binding.Model = "p/removed"
				}
				hl.sessions[scope] = binding
				if state == "other-scope" {
					hl.sessions["other:"+scope] = binding
					delete(hl.sessions, scope)
				}
			}
			before := maps.Clone(hl.sessions)
			attempt := privacy.NewAttempt(policy)
			defer attempt.Close()
			req := privacy.WithAttempt(httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(body)), attempt)
			w := httptest.NewRecorder()
			handleLocal(w, req, cfg, body, nil, hl)
			if w.Code != http.StatusOK {
				t.Fatalf("count failed: %d %s", w.Code, w.Body.String())
			}
			got := rt.State()
			wantMask, wantDetect := 1, 0
			if state == "active" {
				wantMask, wantDetect = 0, 1
			}
			if got.Protected != wantMask || got.Detected != wantDetect {
				t.Fatalf("count selected wrong policy: protected=%d detected=%d, want %d/%d", got.Protected, got.Detected, wantMask, wantDetect)
			}
			if !reflect.DeepEqual(hl.sessions, before) {
				t.Fatal("count created, refreshed or removed a session binding")
			}
		})
	}
}
