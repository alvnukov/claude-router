package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"localrouter/internal/providers"
	codexprovider "localrouter/internal/providers/codex"
)

type codexRequest struct {
	Model             string           `json:"model"`
	Instructions      string           `json:"instructions"`
	Input             []any            `json:"input"`
	Tools             []map[string]any `json:"tools,omitempty"`
	ToolChoice        any              `json:"tool_choice,omitempty"`
	Store             bool             `json:"store"`
	Stream            bool             `json:"stream"`
	Include           []string         `json:"include"`
	PromptCacheKey    string           `json:"prompt_cache_key,omitempty"`
	ParallelToolCalls bool             `json:"parallel_tool_calls"`
	Reasoning         *codexReasoning  `json:"reasoning,omitempty"`
}

// Summary "auto" makes Codex send reasoning summaries while it thinks. They
// reach the client as thinking blocks, and they are bytes from a live model:
// without them a long think is indistinguishable from a dead connection.
type codexReasoning struct {
	Effort  any    `json:"effort,omitempty"`
	Summary string `json:"summary"`
}

// Same stateless Responses mapping used by CozyPhi. Tool call IDs survive the
// round trip so Claude Code can send their outputs on the following turn.
// Only fields Codex CLI itself sends are used (ResponsesApiRequest in
// codex-rs/codex-api/src/common.rs): the subscription endpoint answers 400
// to others such as max_output_tokens, so output limits and sampling
// parameters from the caller are dropped.
func toCodex(req openaiRequest) (codexRequest, error) {
	out := codexRequest{Model: req.Model, Store: false, Stream: true, ParallelToolCalls: true, ToolChoice: "auto",
		Include: []string{"reasoning.encrypted_content"},
		Input:   make([]any, 0, len(req.Messages))}
	out.Reasoning = &codexReasoning{Summary: "auto"}
	if req.ReasoningEffort != "" {
		var effort any = req.ReasoningEffort
		if number, err := strconv.ParseUint(req.ReasoningEffort, 10, 64); err == nil {
			effort = number
		}
		out.Reasoning.Effort = effort
	}
	if req.ParallelToolCalls != nil {
		out.ParallelToolCalls = *req.ParallelToolCalls
	}
	if choice, ok := req.ToolChoice.(map[string]any); ok {
		if fn, ok := choice["function"].(map[string]any); ok {
			out.ToolChoice = map[string]any{"type": "function", "name": fn["name"]}
		}
	} else if req.ToolChoice != nil {
		out.ToolChoice = req.ToolChoice
	}
	knownCalls := map[string]bool{}
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if s, ok := m.Content.(string); ok {
				out.Instructions += s + "\n"
			}
		case "tool":
			if !knownCalls[m.ToolCallID] {
				return out, errors.New("tool result has no matching function call in Codex input")
			}
			content, err := codexContent(m.Content)
			if err != nil {
				return out, err
			}
			out.Input = append(out.Input, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": content})
		case "assistant":
			if m.Content != nil {
				out.Input = append(out.Input, map[string]any{"type": "message", "role": "assistant", "content": m.Content})
			}
			for _, tc := range m.ToolCalls {
				out.Input = append(out.Input, map[string]any{"type": "function_call", "call_id": tc.ID, "name": tc.Function.Name, "arguments": tc.Function.Arguments})
				knownCalls[tc.ID] = true
			}
		case "user":
			content, err := codexContent(m.Content)
			if err != nil {
				return out, err
			}

			out.Input = append(out.Input, map[string]any{"type": "message", "role": "user", "content": content})
		default:
			return out, fmt.Errorf("unsupported role %q", m.Role)
		}
	}
	out.Instructions = strings.TrimSpace(out.Instructions)
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, map[string]any{"type": "function", "name": t.Function.Name,
			"description": t.Function.Description, "parameters": json.RawMessage(t.Function.Parameters)})
	}
	return out, nil
}

type codexResult struct {
	Text  strings.Builder
	Tools []openaiToolCall
	providers.ResponsesUsage
}

func readCodexEvents(body io.Reader) (codexResult, error) {
	result, err := codexprovider.Read(body, nil, nil)
	var out codexResult
	out.Text.WriteString(result.Text)
	out.ResponsesUsage = result.LastUsage
	for _, tool := range result.Tools {
		var call openaiToolCall
		call.ID, call.Type, call.Index = tool.ID, tool.Type, tool.Index
		call.Function.Name, call.Function.Arguments = tool.Function.Name, tool.Function.Arguments
		out.Tools = append(out.Tools, call)
	}
	return out, err
}

func codexAsChatResponse(out codexResult) []byte {
	finish := "stop"
	if len(out.Tools) > 0 {
		finish = "tool_calls"
	}
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": out.Text.String(), "tool_calls": out.Tools}, "finish_reason": finish}},
		"usage":   out.Chat(),
	})
	return b
}

// codexChatStream feeds the existing Anthropic SSE writer one chat-style delta
// at a time. A failed or truncated Codex stream closes the pipe with an error,
// so the caller cannot mistake a partial answer for a completed turn.
func codexChatStream(body io.Reader) io.ReadCloser {
	r, w := io.Pipe()
	go func() { w.CloseWithError(writeCodexChatStream(w, body)) }()
	return r
}

func writeCodexChatStream(w io.Writer, body io.Reader) error {
	return codexprovider.WriteChat(w, body)
}
