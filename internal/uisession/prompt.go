package uisession

import (
	"encoding/json"
	"strings"
)

type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// blocks decodes Anthropic message content: a bare string or a block list.
func blocks(raw json.RawMessage) []block {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []block{{Type: "text", Text: s}}
	}
	var out []block
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// FirstPrompt returns the first meaningful user text of an Anthropic request,
// with client wrapper tags stripped, compacted for a title.
func FirstPrompt(body []byte) string {
	var request struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &request) != nil {
		return ""
	}
	for _, message := range request.Messages {
		if message.Role != "user" {
			continue
		}
		for _, b := range blocks(message.Content) {
			if b.Type != "text" {
				continue
			}
			text := strings.TrimSpace(b.Text)
			for {
				previous := text
				for _, tag := range []string{"system-reminder", "local-command-caveat", "local-command-stdout", "command-name", "command-message", "command-args", "ide_opened_file", "ide_selection"} {
					if strings.HasPrefix(text, "<"+tag+">") {
						_, rest, found := strings.Cut(text, "</"+tag+">")
						if !found {
							text = ""
						} else {
							text = strings.TrimSpace(rest)
						}
					}
				}
				if text == previous {
					break
				}
			}
			if title := compact(text, 120); title != "" {
				return title
			}
		}
	}
	return ""
}

// Facts is what the session list takes from one request body.
type Facts struct{ Effort, Prompt, Preview string }

// ParseFacts reads the requested effort and the first meaningful prompt.
func ParseFacts(body []byte) Facts {
	var request struct {
		OutputConfig struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}
	_ = json.Unmarshal(body, &request)
	return Facts{Effort: request.OutputConfig.Effort, Prompt: FirstPrompt(body)}
}

// Connection names the connection that served a request: Anthropic for the
// cloud and passthrough routes, otherwise the provider part of served.
func Connection(route, served string) string {
	if route == "cloud" || route == "passthrough" {
		return "anthropic"
	}
	connection, _, _ := strings.Cut(served, "/")
	return connection
}
