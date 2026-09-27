package codex

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"localrouter/internal/anthropicerror"
)

func TestClassifyFailureRequiresOriginalHTTP429(t *testing.T) {
	for _, tt := range []struct {
		name, code        string
		original, outward int
		want              anthropicerror.Category
	}{
		{"transient", "rate_limit_exceeded", 429, 429, anthropicerror.Transient429},
		{"slow down", "slow_down", 429, 429, anthropicerror.Transient429},
		{"quota", "insufficient_quota", 429, 429, anthropicerror.Quota429},
		{"credits", "credit_balance_exhausted", 429, 429, anthropicerror.Quota429},
		{"org spend", "organization_spend_limit_exceeded", 429, 429, anthropicerror.Quota429},
		{"project spend", "project_spend_limit_exceeded", 429, 429, anthropicerror.Quota429},
		{"usage excluded", "usage_not_included", 429, 429, anthropicerror.Quota429},
		{"usage reached", "usage_limit_reached", 429, 429, anthropicerror.Quota429},
		{"unknown", "new_private_code", 429, 429, anthropicerror.Unknown429},
		{"empty", "", 429, 429, anthropicerror.Unknown429},
		{"mapped 503", "rate_limit_exceeded", 503, 429, anthropicerror.UpstreamFailure},
		{"mapped quota 503", "insufficient_quota", 503, 429, anthropicerror.UpstreamFailure},
		{"SSE synthetic status", "rate_limit_exceeded", 0, 429, anthropicerror.UpstreamFailure},
		{"disagreeing mapped status", "invalid_prompt", 429, 400, anthropicerror.UpstreamFailure},
		{"wrong final status", "rate_limit_exceeded", 429, 503, anthropicerror.UpstreamFailure},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := upstreamError(tt.code, tt.original)
			e.HTTPStatus = tt.original
			e.validHTTPError = tt.original != 0
			got := ClassifyFailure(fmt.Errorf("wrapped: %w", e), tt.outward)
			if got.Status != tt.outward || got.Category != tt.want || got.HTTPStatus != tt.original || got.RetryAfter != 0 {
				t.Fatalf("classification = %+v; want status %d, category %d, HTTP %d, no retry", got, tt.outward, tt.want, tt.original)
			}
		})
	}
	for _, tt := range []struct {
		name   string
		err    error
		status int
	}{
		{"untyped 429", errors.New("secret upstream message"), 429},
		{"before headers", errors.New("secret upstream message"), 0},
		{"transport", errors.New("secret upstream message"), 502},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyFailure(tt.err, tt.status)
			if got.Status != tt.status || got.Category != anthropicerror.UpstreamFailure || got.HTTPStatus != 0 || got.RetryAfter != 0 {
				t.Fatalf("untyped failure = %+v", got)
			}
		})
	}
}

func TestClassifyFailureValidatesOriginalRetryHeaderIndependently(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name, header, code string
		observed           time.Time
		internal           time.Duration
		want               time.Duration
	}{
		{"seconds", "7", "rate_limit_exceeded", now, 7 * time.Second, 7 * time.Second},
		{"date", now.Add(2 * time.Hour).UTC().Format(http.TimeFormat), "slow_down", now, 0, 0},
		{"25 hours", "90000", "rate_limit_exceeded", now, 25 * time.Hour, 0},
		{"overflow", "18446744073709552", "rate_limit_exceeded", now, 2 * time.Second, 0},
		{"zero", "0", "rate_limit_exceeded", now, 0, 0},
		{"negative", "-1", "rate_limit_exceeded", now, 0, 0},
		{"ambiguous", "7, 8", "rate_limit_exceeded", now, 7 * time.Second, 0},
		{"expired date", now.Add(-time.Hour).UTC().Format(http.TimeFormat), "rate_limit_exceeded", now, 0, 0},
		{"quota", "7", "insufficient_quota", now, 7 * time.Second, 0},
		{"unknown", "7", "unrecognized", now, 7 * time.Second, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := upstreamError(tt.code, 429)
			e.HTTPStatus, e.retryHeader, e.observedAt, e.RetryAfter = 429, tt.header, tt.observed, tt.internal
			e.validHTTPError = true
			got := ClassifyFailure(e, 429)
			if tt.name == "date" {
				if got.RetryAfter < time.Hour || got.RetryAfter > 2*time.Hour {
					t.Fatalf("date duration = %s; want within (1h,2h]", got.RetryAfter)
				}
			} else if got.RetryAfter != tt.want {
				t.Fatalf("public Retry-After = %s; want %s", got.RetryAfter, tt.want)
			}
			if strings.Contains(fmt.Sprint(got), "secret") {
				t.Fatalf("secret in safe failure: %+v", got)
			}
		})
	}
}
