package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"localrouter/internal/anthropicerror"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"localrouter/internal/codextesttransport"
	conf "localrouter/internal/config"
	"localrouter/internal/history"
	"localrouter/internal/privacy"
	"localrouter/internal/providers"
	codexprovider "localrouter/internal/providers/codex"
)

func newID(prefix string) string {
	b := make([]byte, 12)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func writeAnthropicError(w http.ResponseWriter, status int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// A failed write means the client is gone; there is no one to tell.
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": kind, "message": msg},
	})
}

// withoutSignedOut drops Codex members that cannot authorize until someone
// signs in again. If every member is signed out the list is kept, so the
// client still gets the sign-in error.
func withoutSignedOut(cands []candidate) []candidate {
	var out []candidate
	for _, c := range cands {
		if c.Provider.Type == "codex" {
			if s, err := codexStoreFor(c.Provider); err != nil || !s.signedIn() {
				continue
			}
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return cands
	}
	return out
}

// handleLocal serves one /v1/messages call from the OpenAI-compatible endpoint.
// The caller's Anthropic credentials are deliberately not forwarded: the local
// endpoint gets ROUTER_LOCAL_API_KEY and nothing else.
func handleLocal(w http.ResponseWriter, r *http.Request, cfg config, body []byte, tr *history.Trace, hl *health, hist ...*history.Store) {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	oreq, err := toOpenAI(req, "")
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// Try the candidates in order. A model that fails before it has produced a
	// response is skipped for the next one; once bytes have gone to the client
	// the response belongs to that model, and a later failure only counts
	// against its score.
	pickCfg := cfg
	pickCfg.Failover = true
	cands := withoutSignedOut(hl.pick(pickCfg))
	sessionKey := codexprovider.SessionKey(history.SessionOf(body))
	if privacy.FromRequest(r) != nil {
		// Unprotected server-side state must never be selected by the same key.
		sessionKey = ""
	}
	scope := affinityKey(cfg, body, req)
	if r.URL.Path == "/v1/messages/count_tokens" {
		cands = hl.applyExistingAffinity(scope, cands)
	} else {
		cands = hl.bindCandidates(scope, poolRoute{cfg.PoolName, cfg.PoolType}, cands)
	}
	if !cfg.Failover && len(cands) > 1 {
		cands = cands[:1]
	}
	if len(cands) == 0 {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "no local models configured; add one in the router UI")
		return
	}
	var last attemptResult
	for i, cand := range cands {
		input := req
		if p := privacy.FromRequest(r); p != nil {
			masked, maskErr := p.Prepare(privacy.Target{Model: cand.Key, Pool: p.Pool(), Provider: cand.Provider.Name, Translated: true, Protocol: cand.Provider.Type, TokenCount: r.URL.Path == "/v1/messages/count_tokens"}, body)
			if maskErr == nil {
				maskErr = p.CheckControl(cand.Model)
			}
			if maskErr != nil {
				writeAnthropicError(w, 400, "invalid_request_error", "privacy: request rejected")
				return
			}
			if err = json.Unmarshal(masked, &input); err != nil {
				writeAnthropicError(w, 400, "invalid_request_error", "privacy: unsupported request")
				return
			}
		}
		if r.URL.Path == "/v1/messages/count_tokens" {
			// Match the existing local estimate without generating a completion.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": len(body) / 4})
			return
		}
		oreq, err = toOpenAIWithToolImages(input, cand.Model, cand.Provider.Type == "codex")
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if cfg.MaxInputChars > 0 {
			if before, after, notes := fitToBudget(&oreq, cfg.MaxInputChars); before != after && privacy.FromRequest(r) == nil {
				log.Printf("trimmed prompt %d -> %d chars (budget %d): %s", before, after, cfg.MaxInputChars, strings.Join(notes, "; "))
				if tr != nil {
					tr.TrimBefore, tr.TrimAfter, tr.TrimNotes = before, after, notes
				}
			}
		}
		sourceEffort := req.OutputConfig.Effort
		if sourceEffort == "" {
			sourceEffort = "default"
		}
		oreq.ReasoningEffort = cand.Efforts[sourceEffort]
		var payload []byte
		if cand.Provider.Type == "codex" {
			codexInput := oreq
			if privacy.FromRequest(r) != nil {
				codexInput, err = restoreCodexCalls(oreq, "", nil)
				if err != nil {
					writeAnthropicError(w, 400, "invalid_request_error", "privacy: include complete tool call history for Codex")
					return
				}
			} else if len(hist) > 0 {
				codexInput, err = restoreCodexCalls(oreq, history.SessionOf(body), hist[0])
				if err != nil {
					writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
					return
				}
			}
			creq, cerr := toCodex(codexInput)
			if cerr != nil {
				writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", cerr.Error())
				return
			}
			creq.PromptCacheKey = sessionKey
			payload, err = json.Marshal(creq)
		} else {
			payload, err = json.Marshal(oreq)
		}
		if err != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
			return
		}
		if tr != nil {
			tr.OpenAIBody = payload
		}
		hl.acquire(cand.Key)
		var res attemptResult
		if cand.Provider.Type == "codex" {
			res = tryCodexModel(r, cfg, cand, payload, oreq.Stream, sessionKey, hist...)
		} else {
			res = tryModel(r, cfg, cand, payload, oreq.Stream, sessionKey)
		}
		var responseBody io.Reader
		if res.err == nil {
			responseBody = res.resp.Body
		}

		if res.err != nil && privacy.FromRequest(r) != nil {
			res.detail, res.err = privacy.ScrubLocalFailure(r, res.err, res.detail, res.status)
		}
		if tr != nil {
			tr.Attempts = append(tr.Attempts, history.Attempt{Model: cand.Key, Err: res.errMsg(), Dur: res.ttfb,
				Outcome: outcome(r, res, res.err), MaxGap: res.watch.MaxGap()})
		}
		if res.err != nil {
			hl.release(cand.Key)
			last = res
			if res.clientGone {
				return
			}
			hl.record(cand.Key, false, 0, res.err.Error())
			if !res.retryable || i == len(cands)-1 {
				// A member that went quiet mid-think loses the session, so the
				// client's retry goes to the next one.
				if idleMidAnswer(res.err) && i+1 < len(cands) {
					hl.moveSession(scope, cand.Key, cands[i+1])
				}
				break
			}
			log.Printf("local model %s failed (%v), trying %s", cand.Key, res.err, cands[i+1].Key)
			hl.noteFailover(cand.Key, cands[i+1].Key)
			hl.moveSession(scope, cand.Key, cands[i+1])
			continue
		}
		if tr != nil {
			tr.Served = cand.Key
		}
		var werr error
		var usageKnown *bool
		if cand.Provider.Type != "codex" {
			usageKnown = new(bool)
			if tr != nil {
				tr.UsageKnown = usageKnown
			}
		}
		if oreq.Stream {
			werr = streamResponsePolicy(w, responseBody, req.Model, cand.Key, cand.Provider.Type == "codex", usageKnown)
		} else {
			werr = blockingResponsePolicy(w, responseBody, req.Model, cand.Provider.Type == "codex", usageKnown)
		}
		if res.native != nil {
			if werr != nil {
				_ = res.native.Close()
			}
			result, nativeErr := res.native.Result()
			if werr == nil {
				werr = nativeErr
			}
			if werr == nil && tr != nil {
				tr.ProviderState, werr = codexprovider.Capture(res.replayScope, payload, result)
			}
		}
		res.resp.Body.Close()
		res.cancel()
		hl.release(cand.Key)
		if werr != nil && privacy.FromRequest(r) != nil {
			_, werr = privacy.ScrubLocalFailure(r, werr, werr.Error(), res.status)
		}
		if tr != nil {
			a := &tr.Attempts[len(tr.Attempts)-1]
			a.Outcome, a.MaxGap = outcome(r, res, werr), res.watch.MaxGap()
			if werr != nil {
				a.Err = werr.Error()
			}
		}
		if werr != nil {
			// The client went away mid-answer: no failure, and the session stays.
			if r.Context().Err() != nil {
				return
			}
			hl.record(cand.Key, false, 0, werr.Error())
			if cfg.Failover && i+1 < len(cands) {
				hl.moveSession(scope, cand.Key, cands[i+1])
			}
			return
		}
		hl.record(cand.Key, true, res.ttfb, "")
		return
	}
	anthropicerror.WriteHTTP(w, privacy.FailureForResponse(r, last.err, last.status))
}

type attemptResult struct {
	retryAfter  time.Duration
	native      *codexprovider.Stream
	replayScope string
	resp        *http.Response
	cancel      context.CancelFunc
	ttfb        time.Duration
	err         error
	status      int
	detail      string
	retryable   bool
	clientGone  bool
	watch       *bodyWatch // nil when no body arrived
}

// outcome names how an attempt ended, for the history.
func outcome(r *http.Request, res attemptResult, err error) string {
	var protocolErr *codexprovider.ProtocolError
	switch {
	case err == nil:
		return "ok"
	case r.Context().Err() != nil:
		return "client_closed"
	case errors.Is(err, errUpstreamIdle):
		return "upstream_idle"
	case res.status >= 400 || errors.As(err, &protocolErr) && protocolErr.Code != "missing_response_completed":
		return "upstream_error"
	}
	return "upstream_closed"
}

func (a attemptResult) errMsg() string {
	if a.err == nil {
		return ""
	}
	return a.err.Error()
}

// tryModel waits for the first usable response event (not just headers), at most
// cfg.FirstByte. On success the caller owns resp.Body and must call cancel.
func tryModel(r *http.Request, cfg config, cand candidate, payload []byte, stream bool, sessionKey string) attemptResult {
	model := cand.Key
	// Codex answers as a stream whatever the client asked for.
	upstreamStream := stream || cand.Provider.Type == "codex"
	ctx, cancel := context.WithCancel(markStream(r.Context(), upstreamStream))
	endpoint := cand.Provider.BaseURL + "/chat/completions"
	if cand.Provider.Type == "codex" {
		endpoint = conf.CodexBaseURL + "/responses"
	}
	up, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(payload))
	if err != nil {
		cancel()
		return attemptResult{err: err}
	}
	up.Header.Set("Content-Type", "application/json")
	var store *codexAuthStore
	if cand.Provider.Type == "codex" {
		if sessionKey != "" {
			up.Header.Set("session-id", sessionKey)
		}
		if store, err = codexStoreFor(cand.Provider); err != nil {
			cancel()
			return attemptResult{err: err}
		}
		if err := store.authorize(ctx, up); err != nil {
			cancel()
			return attemptResult{err: err, retryable: true}
		}
	} else if cand.Provider.APIKey != "" {
		up.Header.Set("Authorization", "Bearer "+cand.Provider.APIKey)
	}
	if stream {
		up.Header.Set("Accept", "text/event-stream")
	}

	var slow atomic.Bool
	var timer *time.Timer
	if cfg.FirstByte > 0 {
		timer = time.AfterFunc(cfg.FirstByte, func() { slow.Store(true); cancel() })
		defer timer.Stop()
	}
	t0 := time.Now()
	client := http.DefaultClient
	if cand.Provider.Type == "codex" || privacy.FromRequest(r) != nil {
		transport := codextesttransport.Transport()
		if transport == nil {
			transport = upstreamHTTP
		}
		client = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	var resp *http.Response
	if cand.Provider.Type == "codex" {
		resp, err = store.doWithReauth(client, up)
	} else {
		resp, err = client.Do(up)
	}
	ttfb := time.Since(t0)
	if err != nil {
		if privacy.FromRequest(r) != nil {
			err = errors.New("upstream request failed")
		}
		cancel()
		if r.Context().Err() != nil {
			return attemptResult{err: err, clientGone: true}
		}
		if slow.Load() {
			err = fmt.Errorf("%s: no response within %s", model, cfg.FirstByte)
		}
		if errors.Is(err, errCodexSignIn) {
			return attemptResult{err: err, status: http.StatusUnauthorized, detail: err.Error(), ttfb: ttfb, retryable: true}
		}
		return attemptResult{err: err, ttfb: ttfb, retryable: true}
	}
	privacy.ObserveProviderHeaders(r)
	if resp.StatusCode >= 400 || privacy.FromRequest(r) != nil && resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		cancel()
		if privacy.FromRequest(r) != nil {
			detail = []byte("upstream request failed")
		}
		if cand.Provider.Type != "codex" && privacy.FromRequest(r) == nil {
			log.Printf("local endpoint %d (%s): %s", resp.StatusCode, model, detail)
		}
		if cand.Provider.Type == "codex" {
			detail = []byte("Codex request failed")
			if resp.StatusCode == http.StatusUnauthorized {
				detail = []byte(errCodexSignIn.Error())
			}
		}
		res := attemptResult{
			ttfb: ttfb, status: resp.StatusCode, detail: string(detail),
			err: fmt.Errorf("%s: HTTP %d: %s", model, resp.StatusCode, shortDetail(detail)),
		}
		switch resp.StatusCode {
		case 404, 408, 429:
			res.retryable = true
		case 401:
			// A Codex 401 is a sign-in problem of that connection; an
			// OpenAI-compatible 401 is a bad key in the config.
			res.retryable = cand.Provider.Type == "codex"
		default:
			res.retryable = resp.StatusCode >= 500
		}
		return res
	}
	// The watch sits on the raw body: Codex reasoning summaries are bytes
	// from a live model even though no chat event comes of them. A whole
	// answer may keep quiet until it is done, so it only has its gaps measured.
	start, idle := cfg.StartTimeout, cfg.IdleTimeout
	if !upstreamStream {
		start, idle = 0, 0
	}
	original := watchBody(resp.Body, start, idle, func(error) { cancel() })
	var body io.ReadCloser = original
	if p := privacy.FromRequest(r); p != nil && p.MaskExpected() {
		body = &responseReader{Reader: privacy.NewResponseReader(original), close: original.Close}
	}
	if stream && cand.Provider.Type == "codex" {
		body = codexChatStream(body)
	}
	var ready io.Reader
	if stream {
		ready, err = firstChatEvent(body)
	} else {
		reader := bufio.NewReader(body)
		_, err = reader.Peek(1)
		ready = reader
	}
	if timer != nil && !timer.Stop() {
		err = fmt.Errorf("%s: no response within %s", model, cfg.FirstByte)
	}
	if err != nil {
		body.Close()
		original.Close()
		cancel()
		if r.Context().Err() != nil {
			return attemptResult{err: err, clientGone: true, watch: original}
		}
		if slow.Load() {
			err = fmt.Errorf("%s: no response within %s", model, cfg.FirstByte)
		}
		// Silence before the start is retried; silence after the model began
		// is not: another member would make the client wait out a second
		// idle bound.
		return attemptResult{err: err, ttfb: time.Since(t0), retryable: !idleMidAnswer(err), watch: original}
	}
	resp.Body = &responseReader{Reader: ready, close: func() error { body.Close(); return original.Close() }}
	return attemptResult{resp: resp, cancel: cancel, ttfb: time.Since(t0), watch: original}
}

func shortDetail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// inputChars approximates the prompt size the local model will see.
func inputChars(r openaiRequest) int {
	n := 0
	for _, m := range r.Messages {
		switch c := m.Content.(type) {
		case string:
			n += len(c)
		default:
			if b, err := json.Marshal(c); err == nil {
				n += len(b)
			}
		}
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	for _, t := range r.Tools {
		n += len(t.Function.Name) + len(t.Function.Description) + len(t.Function.Parameters)
	}
	return n
}

// ---- non-streaming ----

type openaiResponse struct {
	Choices []struct {
		Message struct {
			Content          string           `json:"content"`
			ReasoningContent string           `json:"reasoning_content"`
			ToolCalls        []openaiToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

func blockingResponse(w http.ResponseWriter, body io.Reader, model string) error {
	return blockingResponsePolicy(w, body, model, false)
}

func blockingResponsePolicy(w http.ResponseWriter, body io.Reader, model string, allowEmpty bool, usageKnown ...*bool) error {
	var or openaiResponse
	var raw json.RawMessage
	decoder := json.NewDecoder(body)
	decodeErr := decoder.Decode(&raw)
	if decodeErr == nil {
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			decodeErr = errors.New("trailing or incomplete upstream response")
		}
	}
	if decodeErr == nil {
		decodeErr = privacy.ValidateObject(raw)
	}
	if decodeErr == nil {
		decodeErr = json.Unmarshal(raw, &or)
	}
	if decodeErr == nil {
		decodeErr = validateChatUsage(or.Usage)
	}
	if err := decodeErr; err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "decode local response: "+err.Error())
		return fmt.Errorf("decode local response: %w", err)
	}
	usage, known := observedChatUsage(or.Usage)
	if len(usageKnown) != 0 && usageKnown[0] != nil {
		*usageKnown[0] = known
	}
	if len(or.Choices) == 0 {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "local endpoint returned no choices")
		return errors.New("local endpoint returned no choices")
	}
	ch := or.Choices[0]

	blocks := []map[string]any{}
	if ch.Message.Content != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": ch.Message.Content})
	}
	for _, tc := range ch.Message.ToolCalls {
		var input any = map[string]any{}
		if tc.Function.Arguments != "" {
			err := privacy.ValidateObject([]byte(tc.Function.Arguments))
			if err == nil {
				err = json.Unmarshal([]byte(tc.Function.Arguments), &input)
			}
			if err != nil {
				writeAnthropicError(w, http.StatusBadGateway, "api_error", "invalid tool arguments; model must correct the JSON")
				return errors.New("invalid tool arguments")
			}
		}
		id := tc.ID
		if id == "" {
			id = newID("toolu_")
		}
		blocks = append(blocks, map[string]any{
			"type": "tool_use", "id": id, "name": tc.Function.Name, "input": input,
		})
	}

	// Апстримная reasoning-модель умеет отвечать целиком внутри reasoning_content,
	// оставляя content и tool_calls пустыми. Клиент Anthropic считает сообщение без
	// блоков отсутствием ответа, поэтому показываем рассуждение вместо пустоты.
	if len(blocks) == 0 && !allowEmpty {
		text := ch.Message.ReasoningContent
		if text == "" {
			text = fmt.Sprintf("(локальная модель не вернула содержимого; finish_reason=%q)", ch.FinishReason)
		}
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}

	w.Header().Set("Content-Type", "application/json")
	// A failed write means the client is gone; there is no one to tell.
	return json.NewEncoder(w).Encode(map[string]any{
		"id":            newID("msg_"),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       blocks,
		"stop_reason":   stopReason(ch.FinishReason),
		"stop_sequence": nil,
		"usage":         usage.Anthropic(),
	})
}

// Client responses may contain synthetic zeroes for compatibility. Only two
// explicit upstream token counters make those zeroes a measured value.
func observedChatUsage(raw json.RawMessage) (providers.ChatUsage, bool) {
	var presence struct {
		PromptTokens     *int `json:"prompt_tokens"`
		CompletionTokens *int `json:"completion_tokens"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &presence) != nil || presence.PromptTokens == nil || presence.CompletionTokens == nil {
		return providers.ChatUsage{}, false
	}
	var usage providers.ChatUsage
	if json.Unmarshal(raw, &usage) != nil {
		return providers.ChatUsage{}, false
	}
	return usage, true
}

// Missing counters mean unknown usage; malformed counters are never silently
// converted to zero. Keep errors free of the raw upstream value.
func validateChatUsage(raw json.RawMessage) error {
	if len(raw) != 0 {
		var usage providers.ChatUsage
		if json.Unmarshal(raw, &usage) != nil {
			return errors.New("invalid upstream usage")
		}
	}
	return nil
}

// ---- streaming ----

type sseWriter struct {
	w   http.ResponseWriter
	f   http.Flusher
	err error
}

func (s *sseWriter) event(name string, payload any) {
	if s.err != nil {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		s.err = err
		return
	}
	_, s.err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, data)
	if s.err == nil {
		s.f.Flush()
	}
}

// failed ends a stream that broke after message_start with an error event
// of the type Claude Code retries. A client that went away gets nothing.
func (s sseWriter) failed(member string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	reason := err.Error()
	var idle *idleError
	if errors.As(err, &idle) {
		reason = idle.Error()
	}
	s.event("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "overloaded_error", "message": member + ": " + reason},
	})
}

type openaiChunk struct {
	Choices []struct {
		Delta struct {
			Content          string           `json:"content"`
			ReasoningContent string           `json:"reasoning_content"`
			ToolCalls        []openaiToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

// streamResponse rewrites an OpenAI delta stream as Anthropic SSE. Anthropic
// keeps at most one content block open at a time, so switching from text to a
// tool call - or between tool calls - closes the previous block first.
func streamResponse(w http.ResponseWriter, body io.Reader, model, member string) error {
	return streamResponsePolicy(w, body, model, member, false)
}

func streamResponsePolicy(w http.ResponseWriter, body io.Reader, model, member string, allowEmpty bool, usageKnown ...*bool) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "streaming unsupported")
		return errors.New("streaming unsupported")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	s := sseWriter{w: w, f: flusher}

	var usage providers.ChatUsage
	known := false
	finish := "stop"
	completed := false

	s.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": newID("msg_"), "type": "message", "role": "assistant",
			"model": model, "content": []any{}, "stop_reason": nil,
			"stop_sequence": nil,
			"usage":         map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
	if s.err != nil {
		return s.err
	}

	nextIndex := 0
	thinkingOpen := false
	openIndex := -1
	textOpen := false
	// toolBlock maps an OpenAI tool_call index to the Anthropic block index we
	// opened for it, so argument deltas land in the right block.
	toolBlock := map[int]int{}

	closeOpen := func() {
		if openIndex >= 0 {
			s.event("content_block_stop", map[string]any{
				"type": "content_block_stop", "index": openIndex,
			})
			openIndex = -1
			textOpen = false
			thinkingOpen = false
		}
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			completed = true
			break
		}
		if data == "" {
			continue
		}

		var chunk openaiChunk
		if err := privacy.ValidateObject([]byte(data)); err != nil {
			return errors.New("invalid upstream stream event")
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return errors.New("invalid upstream stream event")
		}
		if len(chunk.Usage) != 0 {
			if err := validateChatUsage(chunk.Usage); err != nil {
				return err
			}
			usage, known = observedChatUsage(chunk.Usage)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.Delta.ReasoningContent != "" {
			if !thinkingOpen {
				closeOpen()
				openIndex = nextIndex
				nextIndex++
				thinkingOpen = true
				s.event("content_block_start", map[string]any{"type": "content_block_start", "index": openIndex, "content_block": map[string]any{"type": "thinking", "thinking": ""}})
			}
			s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": openIndex, "delta": map[string]any{"type": "thinking_delta", "thinking": choice.Delta.ReasoningContent}})
		}
		if choice.FinishReason != "" {
			finish = choice.FinishReason
			completed = true
		}

		if choice.Delta.Content != "" {
			if !textOpen {
				closeOpen()
				openIndex = nextIndex
				nextIndex++
				textOpen = true
				s.event("content_block_start", map[string]any{
					"type": "content_block_start", "index": openIndex,
					"content_block": map[string]any{"type": "text", "text": ""},
				})
			}
			s.event("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": openIndex,
				"delta": map[string]any{"type": "text_delta", "text": choice.Delta.Content},
			})
		}

		for _, tc := range choice.Delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			blockIdx, seen := toolBlock[idx]
			if !seen {
				closeOpen()
				blockIdx = nextIndex
				nextIndex++
				toolBlock[idx] = blockIdx
				openIndex = blockIdx
				id := tc.ID
				if id == "" {
					id = newID("toolu_")
				}
				s.event("content_block_start", map[string]any{
					"type": "content_block_start", "index": blockIdx,
					"content_block": map[string]any{
						"type": "tool_use", "id": id,
						"name": tc.Function.Name, "input": map[string]any{},
					},
				})
			}
			if tc.Function.Arguments != "" {
				s.event("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": blockIdx,
					"delta": map[string]any{
						"type": "input_json_delta", "partial_json": tc.Function.Arguments,
					},
				})
			}
		}
		if s.err != nil {
			return s.err
		}
	}
	if s.err != nil {
		return s.err
	}
	readErr := scanner.Err()
	if readErr != nil {
		log.Printf("stream read failed")
		s.failed(member, readErr)
		return fmt.Errorf("stream read: %w", readErr)
	}

	if !completed {
		s.failed(member, io.ErrUnexpectedEOF)
		return io.ErrUnexpectedEOF
	}
	if len(usageKnown) != 0 && usageKnown[0] != nil {
		*usageKnown[0] = known
	}

	// An empty completion still needs a content block for the client.
	if nextIndex == 0 && !allowEmpty {
		text := fmt.Sprintf("(модель не вернула содержимого; finish_reason=%q)", finish)
		openIndex = 0
		textOpen = true
		s.event("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		s.event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
	}

	closeOpen()
	s.event("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason": stopReason(finish), "stop_sequence": nil,
		},
		"usage": usage.Anthropic(),
	})
	s.event("message_stop", map[string]any{"type": "message_stop"})
	return s.err
}
