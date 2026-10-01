package codex

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"localrouter/internal/anthropicerror"
)

// ClassifyFailure returns only safe, fixed-category fields for the outward response.
// A mapped status or a decoded SSE error is not proof of an original HTTP 429.
func ClassifyFailure(err error, status int) anthropicerror.Failure {
	f := anthropicerror.Failure{Status: status, Category: anthropicerror.UpstreamFailure}
	var protocol *ProtocolError
	if !errors.As(err, &protocol) {
		return f
	}
	f.HTTPStatus = protocol.HTTPStatus
	if status != http.StatusTooManyRequests || protocol.Status != status || protocol.HTTPStatus != http.StatusTooManyRequests || !protocol.validHTTPError {
		return f
	}
	switch protocol.Code {
	case "rate_limit_exceeded", "slow_down":
		f.Category = anthropicerror.Transient429
		f.RetryAfter = publicRetryAfter(protocol.retryHeader, protocol.observedAt)
	case "insufficient_quota", "credit_balance_exhausted", "organization_spend_limit_exceeded", "project_spend_limit_exceeded", "usage_not_included", "usage_limit_reached", "subscription_sharing_usage_limit_exceeded":
		f.Category = anthropicerror.Quota429
	default:
		f.Category = anthropicerror.Unknown429
	}
	return f
}

// publicRetryAfter never uses ProtocolError.RetryAfter, whose legacy parser
// deliberately continues to control internal behavior independently.
func publicRetryAfter(header string, observedAt time.Time) time.Duration {
	if header == "" || observedAt.IsZero() || len(header) > 128 {
		return 0
	}
	if seconds, err := strconv.ParseUint(header, 10, 64); err == nil {
		if seconds > 0 && seconds <= 24*60*60 && header[0] >= '0' && header[0] <= '9' {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if deadline, err := http.ParseTime(header); err == nil {
		remaining := time.Until(deadline)
		if remaining > 0 && remaining <= 24*time.Hour {
			return remaining
		}
	}
	return 0
}
