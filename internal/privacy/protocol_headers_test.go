package privacy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestProtectedDirectPreservesProtocolHeaders(t *testing.T) {
	for _, mode := range []string{"detect", "mask", "bypass"} {
		for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
			t.Run(mode+path, func(t *testing.T) {
				var calls, local, legacy atomic.Int32
				extra := http.Header{"Anthropic-Dangerous-Direct-Browser-Access": {"true"}, "Anthropic-Required-Unknown": {"future-one", "future-two"}}
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					for name, values := range extra {
						if !reflect.DeepEqual(r.Header.Values(name), values) {
							t.Errorf("protocol header %s changed", name)
						}
					}
					if r.Header.Get("X-Api-Key") != "client-only-synthetic-key" {
						t.Error("authentication changed")
					}
					body, _ := io.ReadAll(r.Body)
					if bytes.Contains(body, []byte(compatCanary)) == (mode == "mask") {
						t.Error("supported content protection changed")
					}
					w.Header().Set("Content-Type", "application/json")
					if path == "/v1/messages/count_tokens" {
						io.WriteString(w, `{"input_tokens":10}`)
					} else {
						io.WriteString(w, `{"content":[]}`)
					}
				}))
				defer up.Close()
				deps := compatHTTPDeps(t, clientCompatRuntime(t, mode), up.URL, "anthropic", &local, &legacy)
				result := compatHTTPCallWithHeaders(t, NewProtectedHTTP(deps), path, clientCompatBody(), extra)
				if result.Code != http.StatusOK || calls.Load() != 1 {
					t.Fatalf("protocol headers rejected: status=%d upstream=%d", result.Code, calls.Load())
				}
			})
		}
	}
}
