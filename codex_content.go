package main

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Codex accepts structured tool outputs, so images must survive the common
// Chat projection. Keep them in that projection until prompt fitting finishes.
func codexToolContent(raw json.RawMessage) (any, error) {
	blocks, err := decodeBlocks(raw)
	if err != nil {
		return nil, err
	}
	var parts []map[string]any
	hasImage := false
	for _, block := range blocks {
		switch block.Type {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": block.Text})
		case "image":
			if block.Source == nil {
				return nil, errors.New("Codex tool image has no source")
			}
			source := block.Source
			url := source.URL
			switch source.Type {
			case "base64":
				url = fmt.Sprintf("data:%s;base64,%s", source.MediaType, source.Data)
			case "url":
			default:
				return nil, errors.New("unsupported Codex tool image source")
			}
			if url == "" {
				return nil, errors.New("Codex tool image has no data")
			}
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			hasImage = true
		default:
			return nil, errors.New("unsupported Codex tool output content")
		}
	}
	if !hasImage {
		return toolResultText(raw), nil
	}
	return parts, nil
}

func codexContent(content any) (any, error) {
	parts, ok := content.([]map[string]any)
	if !ok {
		return content, nil
	}
	converted := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch part["type"] {
		case "text":
			converted = append(converted, map[string]any{"type": "input_text", "text": part["text"]})
		case "image_url":
			image, ok := part["image_url"].(map[string]any)
			if !ok {
				return nil, errors.New("invalid image part")
			}
			converted = append(converted, map[string]any{"type": "input_image", "image_url": image["url"]})
		default:
			return nil, errors.New("unsupported Codex input content")
		}
	}
	return converted, nil
}
