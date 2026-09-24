package main

import (
	"encoding/json"
	"errors"
)

// restoreCodexCalls pairs abbreviated Claude Code tool-result turns with the
// actual tool calls from completed responses in the same session. Codex uses
// stateless Responses requests and requires both sides in the same input.
func restoreCodexCalls(req openaiRequest, session string, history *store) (openaiRequest, error) {
	known := make(map[string]bool)
	out := make([]openaiMsg, 0, len(req.Messages))
	for _, msg := range req.Messages {
		if msg.Role == "tool" && !known[msg.ToolCallID] {
			if session == "" || history == nil || msg.ToolCallID == "" {
				return req, errors.New("Codex tool result has no matching call in this session")
			}
			var call openaiToolCall
			for _, prior := range history.list() {
				if prior.Session != session || !prior.Done() || prior.Status < 200 || prior.Status >= 300 || prior.Resp == nil || prior.Resp.Error != "" || prior.RespTruncated {
					continue
				}
				for _, block := range prior.Resp.Blocks {
					if block.Type == "tool_use" && block.ID == msg.ToolCallID && block.Name != "" && json.Valid([]byte(block.Input)) {
						call.ID, call.Type = block.ID, "function"
						call.Function.Name, call.Function.Arguments = block.Name, block.Input
						break
					}
				}
				if call.ID != "" {
					break
				}
			}
			if call.ID == "" {
				return req, errors.New("Codex tool result has no matching call in this session")
			}
			out = append(out, openaiMsg{Role: "assistant", ToolCalls: []openaiToolCall{call}})
			known[call.ID] = true
		}
		out = append(out, msg)
		if msg.Role == "assistant" {
			for _, call := range msg.ToolCalls {
				known[call.ID] = true
			}
		}
	}
	req.Messages = out
	return req, nil
}
