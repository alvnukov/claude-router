package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"localrouter/internal/buildinfo"
	"localrouter/internal/chatgptplan"
	"localrouter/internal/codextesttransport"
	"localrouter/internal/history"
	"localrouter/internal/privacy"
	codexprovider "localrouter/internal/providers/codex"
)

// tryCodexModel keeps auth and candidate selection at the application boundary;
// the provider owns all native events, continuation and opaque context replay.
func tryCodexModel(r *http.Request, cfg config, cand candidate, visible []byte, streaming bool, sessionKey string, stores ...*history.Store) attemptResult {
	start := time.Now()
	protected := privacy.FromRequest(r) != nil
	if protected {
		// Native replay has no privacy-policy provenance yet. Keep it entirely
		// outside the protected path, including otherwise matching old prefixes.
		sessionKey = ""
		stores = nil
	}
	store, err := codexStoreFor(cand.Provider)
	if err != nil {
		return codexAttemptError(r, start, err)
	}
	credential, err := store.credentialFor(r.Context())
	if err != nil {
		return codexAttemptError(r, start, err)
	}
	account := credential.accountKey()
	planMode := credential.AuthMode == chatgptplan.AuthMode
	endpoint := codexBaseURL + "/responses"
	if planMode {
		endpoint = chatgptplan.ResponsesURL
	}
	scope := ""
	if sessionKey != "" {
		scope = codexprovider.SessionKey(cand.Key + "\x00" + account + "\x00" + sessionKey)
	}
	var records []json.RawMessage
	var replayStore *codexprovider.ReplayStore
	if scope != "" {
		if store.path == "" {
			return codexAttemptError(r, start, errors.New("Codex replay requires a credential storage directory"))
		}
		replayStore, err = codexprovider.NewReplayStore(filepath.Join(filepath.Dir(store.path), "codex-state"))
		if err != nil {
			return codexAttemptError(r, start, err)
		}
		records, err = replayStore.Load(r.Context(), scope)
		if err != nil {
			return codexAttemptError(r, start, err)
		}
	}
	if sessionKey != "" && len(stores) > 0 && stores[0] != nil {
		for _, record := range stores[0].List() {
			if record.Done() && !record.Failed() && record.Served == cand.Key && codexprovider.SessionKey(record.Session) == sessionKey && len(record.ProviderState) > 0 {
				records = append(records, record.ProviderState)
			}
		}
	}
	payload, turnState, err := codexprovider.Restore(scope, visible, records)
	if err != nil {
		return codexAttemptError(r, start, err)
	}
	if planMode {
		payload, err = toChatGPTPlanPayload(payload)
		if err != nil {
			return codexAttemptError(r, start, err)
		}
	}
	client := &http.Client{Transport: codextesttransport.Transport(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	sent := false
	send := func(ctx context.Context, body []byte, headers http.Header) (*http.Response, error) {
		if protected && sent {
			return nil, errors.New("privacy: native continuation requires validated state provenance")
		}
		sent = true
		if planMode {
			var err error
			body, err = toChatGPTPlanPayload(body)
			if err != nil {
				return nil, err
			}
		}
		ctx = context.WithValue(ctx, codexAccountContextKey{}, account)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		request.Header = headers.Clone()
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("User-Agent", buildinfo.UserAgent())
		if planMode {
			for _, key := range []string{"session-id", "thread-id", "x-codex-turn-state", "x-client-request-id", "originator", "ChatGPT-Account-Id", "x-openai-internal-codex-residency"} {
				request.Header.Del(key)
			}
		}
		if err := store.authorize(ctx, request); err != nil {
			return nil, err
		}
		response, err := store.doWithReauth(client, request)
		if err == nil && protected {
			privacy.ObserveProviderHeaders(r)
			if privacy.FromRequest(r).MaskExpected() {
				original := response.Body
				response.Body = &responseReader{Reader: privacy.NewResponseReader(original), close: original.Close}
			}
		}
		return response, err
	}
	ctx, cancel := context.WithCancel(r.Context())
	options := codexprovider.Options{SessionKey: sessionKey, TurnState: turnState, FirstEventTimeout: cfg.firstByte}
	if planMode {
		options.TurnState = ""
	}
	if protected {
		options.ValidateEvent = privacy.ValidateObject
	}
	if replayStore != nil {
		options.BeforeFinish = func(result codexprovider.Completion) error {
			state, err := codexprovider.Capture(scope, visible, result)
			if err != nil {
				return err
			}
			return replayStore.Save(ctx, scope, state)
		}
	}
	native := codexprovider.Start(ctx, payload, send, options, streaming)
	var ready io.Reader
	if streaming {
		ready, err = firstChatEvent(native)
	} else {
		reader := bufio.NewReader(native)
		_, err = reader.Peek(1)
		ready = reader
	}
	if err != nil {
		_ = native.Close()
		cancel()
		_, _ = native.Result()
		return codexAttemptError(r, start, err)
	}
	return attemptResult{resp: &http.Response{StatusCode: http.StatusOK, Body: &responseReader{Reader: ready, close: native.Close}}, cancel: cancel, native: native, replayScope: scope, ttfb: time.Since(start)}
}

func codexAttemptError(r *http.Request, start time.Time, err error) attemptResult {
	result := attemptResult{err: err, ttfb: time.Since(start), clientGone: r.Context().Err() != nil}
	var protocolErr *codexprovider.ProtocolError
	if errors.As(err, &protocolErr) {
		result.status, result.detail, result.retryable = protocolErr.Status, err.Error(), protocolErr.Retryable
		result.retryAfter = protocolErr.RetryAfter
	} else if tokenErr := new(chatgptplan.TokenError); errors.As(err, &tokenErr) {
		result.status, result.detail, result.retryable = tokenErr.Status, err.Error(), tokenErr.Retryable
	} else if errors.Is(err, errCodexSignIn) {
		result.status, result.detail, result.retryable = http.StatusUnauthorized, err.Error(), true
	}
	// Transport errors may follow billable generation. Never automatically replay
	// them on another account; explicit transient server rejections may fail over.
	return result
}
