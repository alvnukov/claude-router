package main

import (
	"bytes"
	"encoding/json"
	"errors"
)

func toChatGPTPlanPayload(body []byte) ([]byte, error) {
	if len(body) > 16<<20 {
		return nil, errors.New("ChatGPT request too large")
	}
	var in map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if dec.Decode(&in) != nil {
		return nil, errors.New("invalid ChatGPT request")
	}
	out := map[string]any{"store": false, "stream": true}
	for _, key := range []string{"model", "instructions", "input", "tools", "tool_choice", "parallel_tool_calls", "include", "reasoning", "prompt_cache_key", "text"} {
		if value, ok := in[key]; ok {
			out[key] = value
		}
	}
	items, ok := out["input"].([]any)
	if !ok {
		return nil, errors.New("ChatGPT requires an input array")
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("invalid ChatGPT input item")
		}
		if item["role"] == "system" {
			return nil, errors.New("ChatGPT requires instructions instead of system input")
		}
		if item["type"] == "function_call" && item["namespace"] == nil {
			item["namespace"] = "functions"
		}
	}
	if raw, exists := out["tools"]; exists {
		tools, ok := raw.([]any)
		if !ok {
			return nil, errors.New("invalid ChatGPT tools")
		}
		flat := []any{}
		groups := []any{}
		names := map[string]bool{}
		hasFunctions := false
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				return nil, errors.New("invalid ChatGPT tool")
			}
			name, _ := tool["name"].(string)
			if name == "" {
				return nil, errors.New("ChatGPT tool name missing")
			}
			if tool["type"] == "namespace" {
				if name == "functions" {
					if hasFunctions {
						return nil, errors.New("duplicate functions namespace")
					}
					hasFunctions = true
				}
				groups = append(groups, tool)
				continue
			}
			if tool["type"] != "function" && tool["type"] != "custom" {
				return nil, errors.New("unsupported ChatGPT tool")
			}
			if names[name] {
				return nil, errors.New("duplicate ChatGPT tool name")
			}
			names[name] = true
			flat = append(flat, tool)
		}
		if len(flat) > 0 {
			if hasFunctions {
				return nil, errors.New("ambiguous functions namespace")
			}
			groups = append(groups, map[string]any{"type": "namespace", "name": "functions", "description": "Tools supplied by the caller.", "tools": flat})
		}
		if len(groups) > 0 {
			out["tools"] = groups
		} else {
			delete(out, "tools")
		}
	}
	return json.Marshal(out)
}
