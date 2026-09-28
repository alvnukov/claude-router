package privacy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestProtectedDirectControlsWithoutApprovedModels(t *testing.T) {
	for _, mode := range []string{"detect", "bypass", "mask"} {
		t.Run(mode, func(t *testing.T) {
			var upstreamCalls, localCalls, legacyCalls atomic.Int32
			body := clientCompatBody()
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				wire, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				var request struct {
					System string `json:"system"`
				}
				if err := json.Unmarshal(wire, &request); err != nil {
					t.Error(err)
					return
				}
				if mode == "mask" {
					if request.System == compatCanary || request.System == "" {
						t.Error("supported text not masked")
					}
				} else if !bytes.Equal(wire, body) {
					t.Error("unmasked request changed")
				}
				for _, control := range []string{`"budget_tokens":31999`, `"keep":"all"`} {
					if !bytes.Contains(wire, []byte(control)) {
						t.Error("controls changed")
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, compatSSE(request.System))
			}))
			defer up.Close()
			rt := clientCompatRuntime(t, mode)
			rt.clientControls = nil // Production has no approved model/beta entries.
			deps := compatHTTPDeps(t, rt, up.URL, "anthropic", &localCalls, &legacyCalls)
			result := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages?beta=true", body)
			if localCalls.Load() != 0 || legacyCalls.Load() != 0 {
				t.Fatal("direct request escaped to another route")
			}
			state := rt.State()
			if result.Code != http.StatusOK || upstreamCalls.Load() != 1 || mode != "mask" && result.Body.String() != compatSSE(compatCanary) {
				t.Fatalf("%s controls rejected: status=%d calls=%d body=%s", mode, result.Code, upstreamCalls.Load(), result.Body.String())
			}
			if mode == "mask" {
				var text, arguments string
				for _, frame := range bytes.Split(result.Body.Bytes(), []byte("\n\n")) {
					event, node, _ := protectedFrameEvent(frame)
					if event != "content_block_delta" || node == nil {
						continue
					}
					delta := node.get("delta")
					if delta == nil {
						continue
					}
					if delta.str("type") == "text_delta" {
						text += delta.str("text")
					}
					if delta.str("type") == "input_json_delta" {
						arguments += delta.str("partial_json")
					}
				}
				if text != "answer "+compatCanary || arguments != `{"query":"`+compatCanary+`"}` {
					t.Fatalf("supported response not restored: text=%q input=%q", text, arguments)
				}
				if state.Protected != 1 || state.Restored != 1 || state.Rejected != 0 || state.Active != 0 {
					t.Fatalf("mask stats: %+v", state)
				}
				return
			}
			if state.Protected != 0 || state.Restored != 0 || state.Rejected != 0 || state.Active != 0 {
				t.Fatalf("unmasked request counted as protected or rejected: %+v", state)
			}
			if mode == "detect" && (state.Detected != 1 || state.Findings[KindOrg] == 0) {
				t.Fatalf("detection did not run: %+v", state)
			}
			if mode == "bypass" && state.Bypassed != 1 {
				t.Fatalf("bypass did not run: %+v", state)
			}
		})
	}
}
