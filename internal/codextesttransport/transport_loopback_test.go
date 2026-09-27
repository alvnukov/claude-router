//go:build router_codex_loopback

package codextesttransport

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPinnedCodexRequestOnlyReachesExplicitLoopback(t *testing.T) {
	var calls atomic.Int32
	var stub *httptest.Server
	stub = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/backend-api/codex/responses" || r.Host != strings.TrimPrefix(stub.URL, "http://") {
			t.Error("synthetic stub received an unexpected request target")
		}
		if r.Header.Get("Authorization") != "Bearer RR_SYNTHETIC.eyJleHAiOjQxMDI0NDQ4MDB9.sig" || r.Header.Get("ChatGPT-Account-Id") != "RR_SYNTHETIC_ACCOUNT" {
			t.Error("test transport replaced or forwarded a non-synthetic credential")
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-Real-Secret") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("test transport copied client-only or unknown headers")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"RR_SYNTHETIC_ERROR"}}`)
	}))
	defer stub.Close()
	t.Setenv("ROUTER_CODEX_TEST_STUB_URL", stub.URL)
	if !DisableBackground() {
		t.Fatal("test binary started background network probes")
	}
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader(`{"synthetic":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer RR_SYNTHETIC.eyJleHAiOjQxMDI0NDQ4MDB9.sig")
	req.Header.Set("ChatGPT-Account-Id", "RR_SYNTHETIC_ACCOUNT")
	req.Header.Set("Cookie", "RR_SYNTHETIC_COOKIE")
	req.Header.Set("X-Real-Secret", "RR_SYNTHETIC_SECRET")
	req.Header.Set("Proxy-Authorization", "RR_SYNTHETIC_PROXY")
	resp, err := Transport().RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || calls.Load() != 1 {
		t.Fatalf("pinned request did not reach exactly one loopback stub: %d calls=%d", resp.StatusCode, calls.Load())
	}
}

func TestCodexLoopbackRejectsWrongOriginAndCredentials(t *testing.T) {
	var calls atomic.Int32
	stub := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer stub.Close()
	t.Setenv("ROUTER_CODEX_TEST_STUB_URL", stub.URL)
	for _, tc := range []struct {
		name, endpoint, auth, account, method string
	}{
		{"wrong-host", "https://example.invalid/backend-api/codex/responses", testToken, testAccount, http.MethodPost},
		{"wrong-scheme", "http://chatgpt.com/backend-api/codex/responses", testToken, testAccount, http.MethodPost},
		{"wrong-path", "https://chatgpt.com/backend-api/codex/other", testToken, testAccount, http.MethodPost},
		{"query", "https://chatgpt.com/backend-api/codex/responses?data=1", testToken, testAccount, http.MethodPost},
		{"refresh-target", "https://auth.openai.com/oauth/token", testToken, testAccount, http.MethodPost},
		{"wrong-method", "https://chatgpt.com/backend-api/codex/responses", testToken, testAccount, http.MethodGet},
		{"real-looking-token", "https://chatgpt.com/backend-api/codex/responses", "Bearer real-account-token", testAccount, http.MethodPost},
		{"real-looking-account", "https://chatgpt.com/backend-api/codex/responses", testToken, "acct-real", http.MethodPost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, tc.endpoint, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", tc.auth)
			req.Header.Set("ChatGPT-Account-Id", tc.account)
			transport := Transport()
			if tc.name == "refresh-target" {
				transport = AuthTransport()
			}
			if resp, err := transport.RoundTrip(req); err == nil {
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				t.Fatal("disallowed target or credential reached test transport")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("rejected request reached stub")
	}
}

func TestCodexLoopbackRejectsUnconfiguredAndNonLoopbackStub(t *testing.T) {
	for _, endpoint := range []string{"", "https://127.0.0.1:12345", "http://localhost:12345", "http://192.0.2.1:12345", "http://127.0.0.1", "http://127.0.0.1:12345/path", "http://127.0.0.1:12345?query=1", "http://127.0.0.1:12345@host.invalid"} {
		t.Run(endpoint, func(t *testing.T) {
			t.Setenv("ROUTER_CODEX_TEST_STUB_URL", endpoint)
			req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", testToken)
			req.Header.Set("ChatGPT-Account-Id", testAccount)
			if resp, err := Transport().RoundTrip(req); err == nil {
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				t.Fatal("unconfigured or external stub URL did not fail closed")
			}
		})
	}
}
