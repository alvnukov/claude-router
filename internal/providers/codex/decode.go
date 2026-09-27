package codex

import (
	"encoding/json"
	"io"
	"strings"
)

type streamedCall struct {
	item      wireItem
	fragments []string
	arguments strings.Builder
}

// Read decodes one native sampling response. Completed items are authoritative;
// unknown output kinds fail explicitly instead of silently losing model work.
// emit receives text immediately and tool argument fragments in complete blocks.
func Read(body io.Reader, emit func(Delta) error, progress func()) (Completion, error) {
	d := decoder{emit: emit, progress: progress, calls: make(map[int]*streamedCall), seen: make(map[string]bool)}
	err := events(body, d.event)
	d.result.Text = d.text.String()
	if err != nil {
		return d.result, err
	}
	// Some compatible upstreams deliver deltas but omit message done items.
	// Retain the visible text in the replay even for that legacy shape.
	if !d.messageDone && d.result.Text != "" {
		b, _ := json.Marshal(map[string]any{"type": "message", "role": "assistant", "content": []TextPart{{Type: "output_text", Text: d.result.Text}}})
		d.result.Output = append(d.result.Output, b)
	}
	return d.result, nil
}

type decoder struct {
	result      Completion
	text        strings.Builder
	textParts   TextStream
	summaries   TextStream
	calls       map[int]*streamedCall
	seen        map[string]bool
	bytes       int
	messageDone bool
	emit        func(Delta) error
	progress    func()
}

func (d *decoder) send(delta Delta) error {
	d.result.Started = true
	if d.progress != nil {
		d.progress()
	}
	if d.emit != nil {
		return d.emit(delta)
	}
	return nil
}

func (d *decoder) event(e wireEvent) (bool, error) {
	switch e.Type {
	case "response.metadata":
		for name, value := range e.Headers {
			if strings.EqualFold(name, "x-codex-turn-state") && d.result.TurnState == "" {
				var state string
				if json.Unmarshal(value, &state) == nil && validHeader(state) {
					d.result.TurnState = state
				}
			}
		}
	case "response.output_text.delta", "response.refusal.delta":
		d.bytes += len(e.Delta)
		if d.bytes > maxProtocolBytes {
			return false, protocolError("response_too_large")
		}
		d.textParts.Delta(e.OutputIndex, e.ContentIndex, e.Delta)
		d.text.WriteString(e.Delta)
		return false, d.send(Delta{Content: e.Delta})
	case "response.reasoning_summary_text.delta":
		d.bytes += len(e.Delta)
		if d.bytes > maxProtocolBytes {
			return false, protocolError("response_too_large")
		}
		d.summaries.Delta(e.OutputIndex, e.SummaryIndex, e.Delta)
		return false, d.send(Delta{ReasoningContent: e.Delta})
	case "response.reasoning_summary_text.done":
		if e.SummaryIndex < 0 || e.SummaryIndex > 1024 {
			return false, protocolError("invalid_summary_index")
		}
		parts := make([]TextPart, e.SummaryIndex+1)
		parts[e.SummaryIndex] = TextPart{Type: "output_text", Text: e.Text}
		rest, err := d.summaries.Done(e.OutputIndex, parts)
		if err != nil {
			return false, err
		}
		if rest != "" {
			return false, d.send(Delta{ReasoningContent: rest})
		}
	case "response.output_item.added":
		var item wireItem
		if json.Unmarshal(e.Item, &item) != nil {
			return false, protocolError("invalid_output_item")
		}
		d.result.Started = true
		if item.Type == "function_call" {
			d.calls[e.OutputIndex] = &streamedCall{item: item}
		}
		if item.Type == "reasoning" && d.progress != nil {
			d.progress()
		}
	case "response.function_call_arguments.delta":
		d.bytes += len(e.Delta)
		if d.bytes > maxProtocolBytes {
			return false, protocolError("response_too_large")
		}
		call := d.calls[e.OutputIndex]
		if call == nil {
			return false, protocolError("arguments_without_function_call")
		}
		call.arguments.WriteString(e.Delta)
		call.fragments = append(call.fragments, e.Delta)
		if d.progress != nil {
			d.progress()
		}
	case "response.output_item.done":
		return false, d.item(e.OutputIndex, e.Item)
	case "response.failed", "error":
		err := e.Response.Error
		if e.Type == "error" {
			err = e.Error
		}
		if err.Code == "" {
			err.Code = err.Type
		}
		return false, upstreamError(err.Code, 502)
	case "response.completed", "response.incomplete":
		if e.Response.ID == "" {
			return false, protocolError("missing_response_id")
		}
		if e.Type == "response.incomplete" {
			if e.Response.Incomplete.Reason != "interrupted" {
				return false, protocolError("response_incomplete")
			}
			value := false
			e.Response.EndTurn = &value
		} else if e.Response.Status != "" && e.Response.Status != "completed" {
			return false, protocolError("invalid_completion_status")
		}
		for i, item := range e.Response.Output {
			if err := d.item(i, item); err != nil {
				return false, err
			}
		}
		if len(d.calls) != 0 {
			return false, protocolError("incomplete_function_call")
		}
		d.result.EndTurn = e.Response.EndTurn
		if e.Response.ID != "" {
			d.result.ResponseIDs = []string{e.Response.ID}
		}
		var usagePresent struct {
			Input  *int `json:"input_tokens"`
			Output *int `json:"output_tokens"`
		}
		if json.Unmarshal(e.Response.Usage, &usagePresent) == nil && usagePresent.Input != nil && usagePresent.Output != nil {
			if json.Unmarshal(e.Response.Usage, &d.result.Usage) != nil {
				return false, protocolError("invalid_usage")
			}
			d.result.LastUsage = d.result.Usage
			d.result.UsageKnown = true
		}
		d.result.Calls = 1
		if d.progress != nil {
			d.progress()
		}
		return true, nil
	default:
		// Codex permits new telemetry and optional delta events. Semantic output
		// is checked at output_item.done, where unsupported items cannot be lost.
	}
	return false, nil
}

func (d *decoder) item(index int, raw json.RawMessage) error {
	d.result.Started = true
	var item wireItem
	if json.Unmarshal(raw, &item) != nil || item.Type == "" {
		return protocolError("invalid_output_item")
	}
	key := item.ID
	if key == "" {
		key = string(raw)
	}
	if d.seen[key] {
		return nil
	}
	d.seen[key] = true
	d.bytes += len(raw)
	if d.bytes > maxProtocolBytes {
		return protocolError("response_too_large")
	}
	switch item.Type {
	case "message":
		if item.Role != "" && item.Role != "assistant" {
			return protocolError("invalid_output_role")
		}
		for _, part := range item.Content {
			if part.Type != "output_text" && part.Type != "refusal" {
				return protocolError("unsupported_output_content")
			}
		}
		rest, err := d.textParts.Done(index, item.Content)
		if err != nil {
			return err
		}
		d.messageDone = true
		d.text.WriteString(rest)
		if rest != "" {
			if err := d.send(Delta{Content: rest}); err != nil {
				return err
			}
		}
	case "function_call":
		if item.CallID == "" || item.Name == "" {
			return protocolError("invalid_function_call")
		}
		if item.Namespace != "" {
			return protocolError("unadvertised_tool_namespace")
		}
		if err := validateArguments(item.Arguments); err != nil {
			return err
		}
		var fragments []string
		if call := d.calls[index]; call != nil {
			if call.item.CallID != item.CallID || call.item.Name != item.Name || !strings.HasPrefix(item.Arguments, call.arguments.String()) {
				return protocolError("function_call_changed")
			}
			fragments = call.fragments
			if suffix := item.Arguments[call.arguments.Len():]; suffix != "" {
				fragments = append(fragments, suffix)
			}
			delete(d.calls, index)
		} else {
			fragments = []string{item.Arguments}
		}
		toolIndex := len(d.result.Tools)
		var tool ToolCall
		tool.ID, tool.Type, tool.Index = item.CallID, "function", &toolIndex
		tool.Function.Name = item.Name
		if err := d.send(Delta{ToolCalls: []ToolCall{tool}}); err != nil {
			return err
		}
		for _, fragment := range fragments {
			var part ToolCall
			part.Index = &toolIndex
			part.Function.Arguments = fragment
			if err := d.send(Delta{ToolCalls: []ToolCall{part}}); err != nil {
				return err
			}
		}
		tool.Function.Arguments = item.Arguments
		d.result.Tools = append(d.result.Tools, tool)
	case "reasoning", "compaction", "compaction_summary", "context_compaction":
		// Opaque signed/encrypted state is replayed byte-for-byte, never exposed
		// as an Anthropic thinking signature or reconstructed from summaries.
	default:
		return protocolError("unsupported_output_item")
	}
	d.result.Output = append(d.result.Output, append(json.RawMessage(nil), raw...))
	if d.progress != nil {
		d.progress()
	}
	return nil
}
