package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Sender owns endpoint pinning and OAuth; the protocol engine owns sequencing.
type Sender func(context.Context, []byte, http.Header) (*http.Response, error)

type Options struct {
	SessionKey        string
	TurnState         string
	FirstEventTimeout time.Duration
	MaxContinuations  int                    // zero uses the bounded default
	BeforeFinish      func(Completion) error // persist completed native state before the client terminal marker
	ValidateEvent     func([]byte) error     // optional transport policy, before lossy JSON decoding
}

// Exchange follows server-requested continuations. It never retries a completed
// sampling call or continues a function call before the client has run its tool.
func Exchange(ctx context.Context, payload []byte, send Sender, options Options, emit func(Delta) error) (Completion, error) {
	var result Completion
	fields, input, err := decodeRequest(payload)
	if err != nil {
		return result, err
	}
	limit := options.MaxContinuations
	if limit <= 0 {
		limit = 32
	}
	result.TurnState = options.TurnState
	seenResponses := make(map[string]bool)
	for call := 0; call <= limit; call++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		headers := make(http.Header)
		headers.Set("Accept", "text/event-stream")
		if options.SessionKey != "" {
			headers.Set("session-id", options.SessionKey)
			headers.Set("thread-id", options.SessionKey)
			headers.Set("x-client-request-id", options.SessionKey)
		}
		if result.TurnState != "" {
			headers.Set("x-codex-turn-state", result.TurnState)
		}
		part, turnState, err := sampling(ctx, payload, headers, send, options.FirstEventTimeout, emit, options.ValidateEvent)
		result.Calls++
		result.Started = result.Started || part.Started
		if err != nil {
			result.UsageKnown = false
			var protocol *ProtocolError
			if (result.Started || result.Calls > 1) && errors.As(err, &protocol) {
				stopped := *protocol
				stopped.Retryable = false
				err = &stopped
			}
			return result, err
		}
		if result.TurnState == "" && validHeader(turnState) {
			result.TurnState = turnState
		}
		for _, id := range part.ResponseIDs {
			if seenResponses[id] {
				return result, protocolError("repeated_response_id")
			}
			seenResponses[id] = true
		}
		result.ResponseIDs = append(result.ResponseIDs, part.ResponseIDs...)
		result.Text += part.Text
		result.Tools = append(result.Tools, part.Tools...)
		result.Output = append(result.Output, part.Output...)
		if rawItemsSize(result.Output) > maxProtocolBytes {
			return result, protocolError("response_too_large")
		}
		if result.Calls == 1 {
			result.Usage = part.Usage
			result.UsageKnown = part.UsageKnown
		} else {
			result.Usage.Add(part.Usage)
			result.UsageKnown = result.UsageKnown && part.UsageKnown
		}
		result.LastUsage = part.LastUsage
		result.EndTurn = part.EndTurn
		if len(part.Tools) != 0 || part.EndTurn == nil || *part.EndTurn {
			return result, nil
		}
		if len(part.Output) == 0 {
			return result, protocolError("continuation_without_progress")
		}
		if call == limit {
			return result, protocolError("continuation_limit_exceeded")
		}
		input = append(input, part.Output...)
		payload, err = encodeRequest(fields, input)
		if err != nil {
			return result, err
		}
	}
	return result, protocolError("continuation_limit_exceeded")
}

func sampling(ctx context.Context, payload []byte, headers http.Header, send Sender, timeout time.Duration, emit func(Delta) error, validate func([]byte) error) (Completion, string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var expired atomic.Bool
	var timer *time.Timer
	if timeout > 0 {
		timer = time.AfterFunc(timeout, func() { expired.Store(true); cancel() })
		defer timer.Stop()
	}
	response, err := send(ctx, payload, headers)
	if err != nil {
		if expired.Load() {
			err = protocolError("first_event_timeout")
		}
		return Completion{}, "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		var body struct {
			Error wireError `json:"error"`
		}
		_ = json.Unmarshal(data, &body)
		err := upstreamError(body.Error.Code, response.StatusCode)
		err.HTTPStatus = response.StatusCode
		var retryValues []string
		for key, values := range response.Header {
			if strings.EqualFold(key, "Retry-After") {
				retryValues = append(retryValues, values...)
			}
		}
		if len(retryValues) == 1 && len(retryValues[0]) <= 128 {
			err.retryHeader = retryValues[0]
			err.observedAt = time.Now()
		}
		if seconds, parseErr := strconv.Atoi(response.Header.Get("Retry-After")); parseErr == nil && seconds > 0 {
			err.RetryAfter = time.Duration(seconds) * time.Second
		} else if reset, parseErr := http.ParseTime(response.Header.Get("Retry-After")); parseErr == nil {
			err.RetryAfter = max(time.Duration(0), time.Until(reset))
		}
		return Completion{}, "", err
	}
	progress := func() {
		if timer != nil {
			timer.Stop()
		}
	}
	result, err := readValidated(response.Body, emit, progress, validate)
	if expired.Load() {
		err = protocolError("first_event_timeout")
	}
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		err = ctx.Err()
	}
	state := response.Header.Get("x-codex-turn-state")
	if state == "" {
		state = result.TurnState
	}
	return result, state, err
}
