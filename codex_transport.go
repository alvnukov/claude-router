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

	"localrouter/internal/history"
	codexprovider "localrouter/internal/providers/codex"
)

// tryCodexModel keeps auth and candidate selection at the application boundary;
// the provider owns all native events, continuation and opaque context replay.
func tryCodexModel(r *http.Request, cfg config, cand candidate, visible []byte, streaming bool, sessionKey string, stores ...*history.Store) attemptResult {
	start := time.Now()
	store, err := codexStoreFor(cand.Provider)
	if err != nil {
		return codexAttemptError(r, start, err)
	}
	credential, err := store.credentialFor(r.Context())
	if err != nil {
		return codexAttemptError(r, start, err)
	}
	account := credential.Tokens.AccountID
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
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	send := func(ctx context.Context, body []byte, headers http.Header) (*http.Response, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, codexBaseURL+"/responses", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		request.Header = headers.Clone()
		request.Header.Set("Content-Type", "application/json")
		if err := store.authorize(ctx, request); err != nil {
			return nil, err
		}
		if request.Header.Get("ChatGPT-Account-Id") != account {
			return nil, errors.New("аккаунт Codex изменился; повторите запрос")
		}
		return store.doWithReauth(client, request)
	}
	ctx, cancel := context.WithCancel(r.Context())
	options := codexprovider.Options{SessionKey: sessionKey, TurnState: turnState, FirstEventTimeout: cfg.firstByte}
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
	} else if errors.Is(err, errCodexSignIn) {
		result.status, result.detail, result.retryable = http.StatusUnauthorized, err.Error(), true
	}
	// Transport errors may follow billable generation. Never automatically replay
	// them on another account; explicit transient server rejections may fail over.
	return result
}
