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
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

func newID(prefix string) string {
	b := make([]byte, 12)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func writeAnthropicError(w http.ResponseWriter, status int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": kind, "message": msg},
	})
}

// handleLocal serves one /v1/messages call from the OpenAI-compatible endpoint.
// The caller's Anthropic credentials are deliberately not forwarded: the local
// endpoint gets ROUTER_LOCAL_API_KEY and nothing else.
func handleLocal(w http.ResponseWriter, r *http.Request, cfg config, body []byte, tr *localTrace, hl *health, history ...*store) {
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

	// Claude Code cannot size its context per model, so the prompt it builds can
	// overrun the local endpoint. Fit it here rather than refusing it: see
	// fitToBudget for why refusing leaves the session with no way out.
	if cfg.maxInputChars > 0 {
		if before, after, notes := fitToBudget(&oreq, cfg.maxInputChars); before != after {
			log.Printf("trimmed prompt %d -> %d chars (budget %d): %s",
				before, after, cfg.maxInputChars, strings.Join(notes, "; "))
			if tr != nil {
				tr.TrimBefore, tr.TrimAfter, tr.TrimNotes = before, after, notes
			}
		}
	}

	// Try the candidates in order. A model that fails before it has produced a
	// response is skipped for the next one; once bytes have gone to the client
	// the response belongs to that model, and a later failure only counts
	// against its score.
	pickCfg := cfg
	pickCfg.failover = true
	cands := hl.pick(pickCfg)
	scope := affinityKey(cfg, body, req)
	cands = hl.bindCandidates(scope, cands)
	if !cfg.failover && len(cands) > 1 {
		cands = cands[:1]
	}
	if len(cands) == 0 {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "no local models configured; add one in the router UI")
		return
	}
	var last attemptResult
	for i, cand := range cands {
		oreq.Model = cand.Model
		sourceEffort := req.OutputConfig.Effort
		if sourceEffort == "" {
			sourceEffort = "default"
		}
		oreq.ReasoningEffort = cand.Efforts[sourceEffort]
		var payload []byte
		if cand.Provider.Type == "codex" {
			codexInput := oreq
			if len(history) > 0 {
				codexInput, err = restoreCodexCalls(oreq, sessionOf(body), history[0])
				if err != nil {
					writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
					return
				}
			}
			creq, err := toCodex(codexInput)
			if err != nil {
				writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
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
		res := tryModel(r, cfg, cand, payload, oreq.Stream)
		var responseBody io.Reader
		if res.err == nil && cand.Provider.Type == "codex" && !oreq.Stream {
			var result codexResult
			result, err = readCodexEvents(res.resp.Body)
			res.resp.Body.Close()
			res.cancel()
			if err != nil {
				res.err, res.retryable = err, true
			}
			responseBody = bytes.NewReader(codexAsChatResponse(result))
		} else if res.err == nil {
			responseBody = res.resp.Body
		}

		if tr != nil {
			tr.Attempts = append(tr.Attempts, attempt{Model: cand.Key, Err: res.errMsg(), Dur: res.ttfb})
		}
		if res.err != nil {
			hl.release(cand.Key)
			last = res
			if res.clientGone {
				return
			}
			hl.record(cand.Key, false, 0, res.err.Error())
			if !res.retryable || i == len(cands)-1 {
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
		if oreq.Stream {
			werr = streamResponse(w, responseBody, req.Model)
		} else {
			werr = blockingResponse(w, responseBody, req.Model)
		}
		res.resp.Body.Close()
		res.cancel()
		hl.release(cand.Key)
		if werr != nil {
			hl.record(cand.Key, false, 0, werr.Error())
			if cfg.failover && i+1 < len(cands) {
				hl.moveSession(scope, cand.Key, cands[i+1])
			}
			if tr != nil {
				tr.Attempts[len(tr.Attempts)-1].Err = werr.Error()
			}
			return
		}
		hl.record(cand.Key, true, res.ttfb, "")
		return
	}
	if last.status != 0 {
		writeAnthropicError(w, last.status, "api_error",
			fmt.Sprintf("local endpoint returned %d: %s", last.status, last.detail))
		return
	}
	writeAnthropicError(w, http.StatusBadGateway, "api_error", "local endpoint: "+last.errMsg())
}

type attemptResult struct {
	resp       *http.Response
	cancel     context.CancelFunc
	ttfb       time.Duration
	err        error
	status     int
	detail     string
	retryable  bool
	clientGone bool
}

func (a attemptResult) errMsg() string {
	if a.err == nil {
		return ""
	}
	return a.err.Error()
}

// tryModel waits for the first usable response event (not just headers), at most
// cfg.firstByte. On success the caller owns resp.Body and must call cancel.
func tryModel(r *http.Request, cfg config, cand candidate, payload []byte, stream bool) attemptResult {
	model := cand.Key
	ctx, cancel := context.WithCancel(r.Context())
	endpoint := cand.Provider.BaseURL + "/chat/completions"
	if cand.Provider.Type == "codex" {
		endpoint = codexBaseURL + "/responses"
	}
	up, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(payload))
	if err != nil {
		cancel()
		return attemptResult{err: err}
	}
	up.Header.Set("Content-Type", "application/json")
	var store *codexAuthStore
	if cand.Provider.Type == "codex" {
		if store, err = codexStoreFor(cand.Provider); err != nil {
			cancel()
			return attemptResult{err: err}
		}
		if err := store.authorize(ctx, up); err != nil {
			cancel()
			return attemptResult{err: err}
		}
	} else if cand.Provider.APIKey != "" {
		up.Header.Set("Authorization", "Bearer "+cand.Provider.APIKey)
	}
	if stream {
		up.Header.Set("Accept", "text/event-stream")
	}

	var slow atomic.Bool
	var timer *time.Timer
	if cfg.firstByte > 0 {
		timer = time.AfterFunc(cfg.firstByte, func() { slow.Store(true); cancel() })
		defer timer.Stop()
	}
	t0 := time.Now()
	client := http.DefaultClient
	if cand.Provider.Type == "codex" {
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	var resp *http.Response
	if cand.Provider.Type == "codex" {
		resp, err = store.doWithReauth(client, up)
	} else {
		resp, err = client.Do(up)
	}
	ttfb := time.Since(t0)
	if err != nil {
		cancel()
		if r.Context().Err() != nil {
			return attemptResult{err: err, clientGone: true}
		}
		if slow.Load() {
			err = fmt.Errorf("%s: no response within %s", model, cfg.firstByte)
		}
		if errors.Is(err, errCodexSignIn) {
			return attemptResult{err: err, status: http.StatusUnauthorized, detail: err.Error(), ttfb: ttfb}
		}
		return attemptResult{err: err, ttfb: ttfb, retryable: true}
	}
	if resp.StatusCode >= 400 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		cancel()
		if cand.Provider.Type != "codex" {
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
		default:
			res.retryable = resp.StatusCode >= 500
		}
		return res
	}
	original := resp.Body
	var body io.ReadCloser = original
	if stream && cand.Provider.Type == "codex" {
		body = codexChatStream(original)
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
		err = fmt.Errorf("%s: no response within %s", model, cfg.firstByte)
	}
	if err != nil {
		body.Close()
		original.Close()
		cancel()
		if r.Context().Err() != nil {
			return attemptResult{err: err, clientGone: true}
		}
		if slow.Load() {
			err = fmt.Errorf("%s: no response within %s", model, cfg.firstByte)
		}
		return attemptResult{err: err, ttfb: time.Since(t0), retryable: true}
	}
	resp.Body = &responseReader{Reader: ready, close: func() error { body.Close(); return original.Close() }}
	return attemptResult{resp: resp, cancel: cancel, ttfb: time.Since(t0)}
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
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func blockingResponse(w http.ResponseWriter, body io.Reader, model string) error {
	var or openaiResponse
	if err := json.NewDecoder(body).Decode(&or); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "decode local response: "+err.Error())
		return fmt.Errorf("decode local response: %w", err)
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
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
				input = map[string]any{}
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
	if len(blocks) == 0 {
		text := ch.Message.ReasoningContent
		if text == "" {
			text = fmt.Sprintf("(локальная модель не вернула содержимого; finish_reason=%q)", ch.FinishReason)
		}
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":            newID("msg_"),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       blocks,
		"stop_reason":   stopReason(ch.FinishReason),
		"stop_sequence": nil,
		"usage": map[string]int{
			"input_tokens":  or.Usage.PromptTokens,
			"output_tokens": or.Usage.CompletionTokens,
		},
	})
	return nil
}

// ---- streaming ----

type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (s sseWriter) event(name string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, data)
	s.f.Flush()
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
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// streamResponse rewrites an OpenAI delta stream as Anthropic SSE. Anthropic
// keeps at most one content block open at a time, so switching from text to a
// tool call - or between tool calls - closes the previous block first.
func streamResponse(w http.ResponseWriter, body io.Reader, model string) error {
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

	inTokens, outTokens := 0, 0
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
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			inTokens = chunk.Usage.PromptTokens
			outTokens = chunk.Usage.CompletionTokens
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
	}
	readErr := scanner.Err()
	if readErr != nil {
		log.Printf("stream read: %v", readErr)
		return fmt.Errorf("stream read: %w", readErr)
	}

	if !completed {
		return io.ErrUnexpectedEOF
	}

	// An empty completion still needs a content block for the client.
	if nextIndex == 0 {
		text := fmt.Sprintf("(модель не вернула содержимого; finish_reason=%q)", finish)
		openIndex = 0
		nextIndex = 1
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
		"usage": map[string]int{"input_tokens": inTokens, "output_tokens": outTokens},
	})
	s.event("message_stop", map[string]any{"type": "message_stop"})
	return nil
}
