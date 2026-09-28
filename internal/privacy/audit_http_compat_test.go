package privacy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrivacyAuditRepeatedBetaAndEquivalentQuery(t *testing.T) {
	for _, mode := range []string{"mask", "detect", "bypass"} {
		for _, query := range []string{"beta=true", "%62eta=%74rue", "beta=true&"} {
			t.Run(mode+"/"+query, func(t *testing.T) {
				var calls, local, legacy atomic.Int32
				betas := []string{"feature-one", "feature-two"}
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.RawQuery != query || !reflect.DeepEqual(r.Header.Values("Anthropic-Beta"), betas) {
						t.Error("protocol parameters changed")
					}
					io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"content":[]}`)
				}))
				defer up.Close()
				deps := compatHTTPDeps(t, clientCompatRuntime(t, mode), up.URL, "anthropic", &local, &legacy)
				result := compatHTTPCallWithHeaders(t, NewProtectedHTTP(deps), "/v1/messages?"+query, clientCompatBody(), http.Header{"Anthropic-Beta": betas})
				if result.Code != 200 || calls.Load() != 1 {
					t.Fatalf("valid beta rejected: status=%d calls=%d", result.Code, calls.Load())
				}
			})
		}
	}
}

func TestPrivacyAuditDirectErrorStatus(t *testing.T) {
	for _, mode := range []string{"mask", "detect", "bypass"} {
		for _, code := range []int{400, 401, 403, 404, 413, 429, 500, 503, 529} {
			t.Run(mode+"/"+strconv.Itoa(code), func(t *testing.T) {
				var local, legacy atomic.Int32
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					w.Header().Set("Retry-After", "7")
					w.Header().Set("Set-Cookie", compatCanary)
					w.WriteHeader(code)
					io.WriteString(w, compatCanary)
				}))
				defer up.Close()
				deps := compatHTTPDeps(t, clientCompatRuntime(t, mode), up.URL, "anthropic", &local, &legacy)
				result := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages", clientCompatBody())
				if result.Code != code {
					t.Fatalf("upstream %d became %d", code, result.Code)
				}
				wantType := map[int]string{400: "invalid_request_error", 401: "authentication_error", 403: "permission_error", 404: "not_found_error", 413: "request_too_large", 429: "rate_limit_error", 529: "overloaded_error"}[code]
				if wantType == "" {
					wantType = "api_error"
				}
				if !strings.Contains(result.Body.String(), `"type":"`+wantType+`"`) || strings.Contains(result.Body.String(), compatCanary) || result.Header.Get("Set-Cookie") != "" {
					t.Fatal("unsafe or incompatible provider error")
				}
				if (code == 429 || code == 503 || code == 529) && result.Header.Get("Retry-After") != "7" {
					t.Fatal("retry delay lost")
				}
			})
		}
	}
}

func TestPrivacyAuditDirectErrorRetryHeaderIsBounded(t *testing.T) {
	date := time.Now().Add(5 * time.Minute).UTC().Format(http.TimeFormat)
	for _, tc := range []struct {
		values []string
		want   string
	}{
		{[]string{compatCanary}, ""}, {[]string{"7", "8"}, ""}, {[]string{"86401"}, ""}, {[]string{"-1"}, ""}, {[]string{"99999999999999999999999"}, ""},
		{[]string{"7"}, "7"}, {[]string{"0"}, "0"}, {[]string{"86400"}, "86400"}, {[]string{date}, date}, {[]string{"Sun, 06 Nov 1994 08:49:37 GMT"}, ""},
	} {
		values := tc.values
		t.Run(strings.Join(values, "/"), func(t *testing.T) {
			var local, legacy atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header()["Retry-After"] = values
				w.WriteHeader(429)
				io.WriteString(w, compatCanary)
			}))
			defer up.Close()
			deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), up.URL, "anthropic", &local, &legacy)
			result := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages", clientCompatBody())
			want := tc.want
			if result.Code != 429 || result.Header.Get("Retry-After") != want || strings.Contains(result.Body.String(), compatCanary) {
				t.Fatal("unsafe retry information escaped")
			}
		})
	}
}

func TestPrivacyAuditExpiredDirectError(t *testing.T) {
	var local, legacy atomic.Int32
	deps := compatHTTPDeps(t, clientCompatRuntime(t, "mask"), "http://synthetic.invalid", "anthropic", &local, &legacy)
	clock := newCompatClock()
	deps.Clock = clock
	deps.Client = &http.Client{Transport: compatRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		clock.advance(deps.Limits.Total)
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"7"}}, Body: io.NopCloser(strings.NewReader("raw")), Request: r}, nil
	})}
	result := compatHTTPCall(t, NewProtectedHTTP(deps), "/v1/messages", compatHTTPBody())
	if result.Code != 502 || result.Header.Get("Retry-After") != "" {
		t.Fatalf("expired request released provider response: %d %q", result.Code, result.Header.Get("Retry-After"))
	}
}
