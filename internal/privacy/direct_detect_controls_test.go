package privacy

import (
	"bytes"
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
				if err != nil || !bytes.Equal(wire, body) {
					t.Error("unmasked request changed", err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, compatSSE(compatCanary))
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
			if mode == "mask" {
				if result.Code != http.StatusBadRequest || upstreamCalls.Load() != 0 || state.Rejected != 1 {
					t.Fatalf("unapproved masking controls escaped: status=%d calls=%d", result.Code, upstreamCalls.Load())
				}
				return
			}
			if result.Code != http.StatusOK || upstreamCalls.Load() != 1 || result.Body.String() != compatSSE(compatCanary) {
				t.Fatalf("%s controls rejected: status=%d calls=%d", mode, result.Code, upstreamCalls.Load())
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
