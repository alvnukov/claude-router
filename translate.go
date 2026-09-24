package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ---- Anthropic request shapes (only the fields we act on) ----

type anthropicRequest struct {
	Model         string          `json:"model"`
	System        json.RawMessage `json:"system,omitempty"`
	Messages      []anthropicMsg  `json:"messages"`
	Tools         []anthropicTool `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	OutputConfig  struct {
		Effort string `json:"effort,omitempty"`
	} `json:"output_config,omitempty"`
	Stream bool `json:"stream,omitempty"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Source    *imageSource    `json:"source,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

// ---- OpenAI request shapes ----

type openaiRequest struct {
	Model           string         `json:"model"`
	Messages        []openaiMsg    `json:"messages"`
	Tools           []openaiTool   `json:"tools,omitempty"`
	ToolChoice      any            `json:"tool_choice,omitempty"`
	MaxTokens       int            `json:"max_tokens,omitempty"`
	Temperature     *float64       `json:"temperature,omitempty"`
	TopP            *float64       `json:"top_p,omitempty"`
	Stop            []string       `json:"stop,omitempty"`
	Stream          bool           `json:"stream,omitempty"`
	StreamOpts      *streamOptions `json:"stream_options,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openaiMsg struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"`
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Index    *int   `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// decodeBlocks accepts both content encodings Anthropic allows: a bare string
// or an array of typed blocks.
func decodeBlocks(raw json.RawMessage) ([]contentBlock, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []contentBlock{{Type: "text", Text: s}}, nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}
	return blocks, nil
}

func systemText(raw json.RawMessage) string {
	blocks, err := decodeBlocks(raw)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" && blk.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// toolResultText flattens a tool_result payload, which may itself be a string
// or a block array, into the plain string OpenAI's tool role expects.
func toolResultText(raw json.RawMessage) string {
	blocks, err := decodeBlocks(raw)
	if err != nil {
		return string(raw)
	}
	var parts []string
	for _, blk := range blocks {
		if blk.Text != "" {
			parts = append(parts, blk.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func toOpenAI(req anthropicRequest, model string) (openaiRequest, error) {
	out := openaiRequest{
		Model:       model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.StopSequences,
		Stream:      req.Stream,
	}
	if req.Stream {
		out.StreamOpts = &streamOptions{IncludeUsage: true}
	}

	if sys := systemText(req.System); sys != "" {
		out.Messages = append(out.Messages, openaiMsg{Role: "system", Content: sys})
	}

	for _, m := range req.Messages {
		blocks, err := decodeBlocks(m.Content)
		if err != nil {
			return out, err
		}

		// tool_result blocks become their own `tool` messages and must precede
		// whatever else the same user turn carried.
		for _, blk := range blocks {
			if blk.Type == "tool_result" {
				out.Messages = append(out.Messages, openaiMsg{
					Role:       "tool",
					ToolCallID: blk.ToolUseID,
					Content:    toolResultText(blk.Content),
				})
			}
		}

		var text strings.Builder
		var parts []map[string]any
		var calls []openaiToolCall

		for _, blk := range blocks {
			switch blk.Type {
			case "text":
				if text.Len() > 0 {
					text.WriteString("\n")
				}
				text.WriteString(blk.Text)
			case "image":
				if blk.Source == nil {
					continue
				}
				u := blk.Source.URL
				if blk.Source.Type == "base64" {
					u = fmt.Sprintf("data:%s;base64,%s", blk.Source.MediaType, blk.Source.Data)
				}
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": u},
				})
			case "tool_use":
				var c openaiToolCall
				c.ID = blk.ID
				c.Type = "function"
				c.Function.Name = blk.Name
				args := string(blk.Input)
				if args == "" || args == "null" {
					args = "{}"
				}
				c.Function.Arguments = args
				calls = append(calls, c)
			}
			// "thinking" and other block types carry no equivalent and are dropped.
		}

		if text.Len() == 0 && len(parts) == 0 && len(calls) == 0 {
			continue
		}

		// Апстрим ломается на системном сообщении в середине или в конце диалога:
		// рассуждение обнуляется, а иногда ответ пуст целиком, и клиент Anthropic
		// видит сообщение без блоков. Anthropic доставляет такие напоминания внутри
		// пользовательского хода, поэтому меняем роль, сохраняя место в диалоге.
		role := m.Role
		if role == "system" {
			role = "user"
		}
		msg := openaiMsg{Role: role}
		switch {
		case len(parts) > 0:
			if text.Len() > 0 {
				parts = append([]map[string]any{{"type": "text", "text": text.String()}}, parts...)
			}
			msg.Content = parts
		case text.Len() > 0:
			msg.Content = text.String()
		}
		msg.ToolCalls = calls
		out.Messages = append(out.Messages, msg)
	}

	for _, t := range req.Tools {
		var ot openaiTool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.InputSchema
		out.Tools = append(out.Tools, ot)
	}

	out.ToolChoice = convertToolChoice(req.ToolChoice)
	return out, nil
}

func convertToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": tc.Name},
		}
	}
	return nil
}

func stopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	default:
		return "end_turn"
	}
}
