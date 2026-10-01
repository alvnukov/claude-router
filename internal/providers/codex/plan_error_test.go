package codex

import (
	"context"
	"errors"
	"io"
	"localrouter/internal/anthropicerror"
	"net/http"
	"strings"
	"testing"
)

func TestChatGPTPlanPartialLimitNeverRetries(t *testing.T) {
	result, err := Exchange(context.Background(), []byte(`{"model":"gpt-6.1-sol","input":[]}`), func(context.Context, []byte, http.Header) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"subscription_sharing_usage_limit_exceeded\"}}}\n\n"))}, nil
	}, Options{}, func(Delta) error { return nil })
	var protocol *ProtocolError
	if !errors.As(err, &protocol) || protocol.Code != "subscription_sharing_usage_limit_exceeded" || protocol.Retryable || !result.Started || result.Calls != 1 {
		t.Fatalf("partial limit restarted or disappeared: %+v %v", result, err)
	}
}
func TestChatGPTPlanRejectsUnadvertisedNamespacedFunctions(t *testing.T) {
	for _, tc := range []struct{ namespace, name string }{{"evil", "lookup"}, {"functions", "unadvertised"}} {
		_, err := Exchange(context.Background(), []byte(`{"input":[],"tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"lookup"}]}]}`), func(context.Context, []byte, http.Header) (*http.Response, error) {
			body := `data: {"type":"response.output_item.done","item":{"type":"function_call","call_id":"call","namespace":"` + tc.namespace + `","name":"` + tc.name + `","arguments":"{}"}}` + "\n\n"
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		}, Options{}, nil)
		if err == nil || !strings.Contains(err.Error(), "unadvertised_tool_namespace") {
			t.Fatalf("unadvertised tool accepted: %v", err)
		}
	}
}

func TestChatGPTPlanErrorsPreserveRecovery(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
		retry  bool
	}{
		{"subscription_sharing_usage_limit_exceeded", 429, true}, {"subscription_sharing_usage_unavailable", 503, true}, {"subscription_sharing_user_not_eligible", 403, false}, {"subscription_sharing_unsupported_capability", 400, false}, {"subscription_sharing_route_not_supported", 403, false}, {"subscription_sharing_invalid_user", 401, false},
	} {
		e := upstreamError(tc.code, tc.status)
		if e.Code != tc.code || e.Status != tc.status || e.Retryable != tc.retry {
			t.Errorf("lost plan error %s: %+v", tc.code, e)
		}
		if tc.status == 429 {
			e.HTTPStatus = 429
			e.validHTTPError = true
			if ClassifyFailure(e, 429).Category != anthropicerror.Quota429 {
				t.Error("plan limit not classified as quota")
			}
		}
	}
}
