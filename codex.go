package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type codexRequest struct {
	Model        string           `json:"model"`
	Instructions string           `json:"instructions,omitempty"`
	Input        []any            `json:"input"`
	Tools        []map[string]any `json:"tools,omitempty"`
	ToolChoice   any              `json:"tool_choice,omitempty"`
	Store        bool             `json:"store"`
	Stream       bool             `json:"stream"`
	Include      []string         `json:"include"`
	Reasoning    *struct {
		Effort string `json:"effort"`
	} `json:"reasoning,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
}

// Same stateless Responses mapping used by CozyPhi. Tool call IDs survive the
// round trip so Claude Code can send their outputs on the following turn.
func toCodex(req openaiRequest) (codexRequest, error) {
	out := codexRequest{Model: req.Model, Store: false, Stream: true,
		Include:     []string{"reasoning.encrypted_content"},
		Temperature: req.Temperature, TopP: req.TopP,
		Input: make([]any, 0, len(req.Messages))}
	if req.ReasoningEffort != "" {
		out.Reasoning = &struct {
			Effort string `json:"effort"`
		}{Effort: req.ReasoningEffort}
		out.Temperature, out.TopP = nil, nil
	}
	if choice, ok := req.ToolChoice.(map[string]any); ok {
		if fn, ok := choice["function"].(map[string]any); ok {
			out.ToolChoice = map[string]any{"type": "function", "name": fn["name"]}
		}
	} else {
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
			out.Input = append(out.Input, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": m.Content})
		case "assistant":
			if m.Content != nil {
				out.Input = append(out.Input, map[string]any{"type": "message", "role": "assistant", "content": m.Content})
			}
			for _, tc := range m.ToolCalls {
				out.Input = append(out.Input, map[string]any{"type": "function_call", "call_id": tc.ID, "name": tc.Function.Name, "arguments": tc.Function.Arguments})
				knownCalls[tc.ID] = true
			}
		case "user":
			content := m.Content
			if parts, ok := content.([]map[string]any); ok {
				converted := make([]map[string]any, 0, len(parts))
				for _, part := range parts {
					switch part["type"] {
					case "text":
						converted = append(converted, map[string]any{"type": "input_text", "text": part["text"]})
					case "image_url":
						img, ok := part["image_url"].(map[string]any)
						if !ok {
							return out, errors.New("invalid image part")
						}
						converted = append(converted, map[string]any{"type": "input_image", "image_url": img["url"]})
					}
				}
				content = converted
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

type codexEvent struct {
	Type        string `json:"type"`
	Delta       string `json:"delta"`
	OutputIndex int    `json:"output_index"`
	Item        struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	Response struct {
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"response"`
}

type codexResult struct {
	Text         strings.Builder
	Tools        []openaiToolCall
	InputTokens  int
	OutputTokens int
}

func readCodexEvents(body io.Reader) (codexResult, error) {
	var out codexResult
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), 10<<20)
	complete := false
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var e codexEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return out, fmt.Errorf("decode Codex event: %w", err)
		}
		switch e.Type {
		case "response.output_text.delta":
			out.Text.WriteString(e.Delta)
		case "response.output_item.done":
			if e.Item.Type == "function_call" {
				var tc openaiToolCall
				tc.ID, tc.Type = e.Item.CallID, "function"
				tc.Function.Name, tc.Function.Arguments = e.Item.Name, e.Item.Arguments
				out.Tools = append(out.Tools, tc)
			}
		case "response.completed":
			if e.Response.Status != "" && e.Response.Status != "completed" {
				return out, fmt.Errorf("Codex response status: %s", e.Response.Status)
			}
			out.InputTokens, out.OutputTokens = e.Response.Usage.InputTokens, e.Response.Usage.OutputTokens
			complete = true
		case "response.failed", "response.incomplete", "error":
			return out, errors.New("Codex response did not complete")
		}
	}
	if err := scanner.Err(); err != nil {
		return out, fmt.Errorf("read Codex response: %w", err)
	}
	if !complete {
		return out, errors.New("Codex stream ended before response.completed")
	}
	return out, nil
}

func codexAsChatResponse(out codexResult) []byte {
	finish := "stop"
	if len(out.Tools) > 0 {
		finish = "tool_calls"
	}
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": out.Text.String(), "tool_calls": out.Tools}, "finish_reason": finish}},
		"usage":   map[string]int{"prompt_tokens": out.InputTokens, "completion_tokens": out.OutputTokens},
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
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), 10<<20)
	toolIndex := 0
	type streamedTool struct {
		index int
		args  string
	}
	tools := map[int]*streamedTool{}
	completed := false
	write := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", b)
		return err
	}
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var e codexEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return fmt.Errorf("decode Codex event: %w", err)
		}
		switch e.Type {
		case "response.output_text.delta":
			if err := write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": e.Delta}}}}); err != nil {
				return err
			}
		case "response.output_item.added":
			if e.Item.Type != "function_call" || e.Item.CallID == "" || e.Item.Name == "" {
				continue
			}
			i := toolIndex
			toolIndex++
			tools[e.OutputIndex] = &streamedTool{index: i}
			var tc openaiToolCall
			tc.ID, tc.Type, tc.Index = e.Item.CallID, "function", &i
			tc.Function.Name = e.Item.Name
			if err := write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []openaiToolCall{tc}}}}}); err != nil {
				return err
			}
		case "response.function_call_arguments.delta":
			tool := tools[e.OutputIndex]
			if tool == nil || e.Delta == "" {
				continue
			}
			tool.args += e.Delta
			i := tool.index
			var tc openaiToolCall
			tc.Index = &i
			tc.Function.Arguments = e.Delta
			if err := write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []openaiToolCall{tc}}}}}); err != nil {
				return err
			}
		case "response.output_item.done":
			if e.Item.Type != "function_call" {
				continue
			}
			if tool := tools[e.OutputIndex]; tool != nil {
				if !strings.HasPrefix(e.Item.Arguments, tool.args) {
					return errors.New("Codex tool arguments changed during streaming")
				}
				if rest := strings.TrimPrefix(e.Item.Arguments, tool.args); rest != "" {
					i := tool.index
					var tc openaiToolCall
					tc.Index = &i
					tc.Function.Arguments = rest
					if err := write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []openaiToolCall{tc}}}}}); err != nil {
						return err
					}
				}
				delete(tools, e.OutputIndex)
				continue
			}
			i := toolIndex
			toolIndex++
			var tc openaiToolCall
			tc.ID, tc.Type, tc.Index = e.Item.CallID, "function", &i
			tc.Function.Name, tc.Function.Arguments = e.Item.Name, e.Item.Arguments
			if err := write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []openaiToolCall{tc}}}}}); err != nil {
				return err
			}
		case "response.completed":
			if e.Response.Status != "" && e.Response.Status != "completed" {
				return errors.New("Codex response did not complete")
			}
			finish := "stop"
			if toolIndex > 0 {
				finish = "tool_calls"
			}
			if err := write(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": finish}}}); err != nil {
				return err
			}
			if err := write(map[string]any{"usage": map[string]int{"prompt_tokens": e.Response.Usage.InputTokens, "completion_tokens": e.Response.Usage.OutputTokens}}); err != nil {
				return err
			}
			completed = true
		case "response.failed", "response.incomplete", "error":
			return errors.New("Codex response did not complete")
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Codex response: %w", err)
	}
	if !completed {
		return errors.New("Codex stream ended before response.completed")
	}
	return nil
}
