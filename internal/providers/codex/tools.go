package codex

import "encoding/json"

// Only a function actually advertised by this request may use a namespace.
// Standalone legacy decoding continues to reject every namespaced call.
func advertisedFunctions(payload []byte) map[string]map[string]bool {
	var request struct {
		Tools []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Tools []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"tools"`
	}
	allowed := map[string]map[string]bool{}
	if json.Unmarshal(payload, &request) != nil {
		return allowed
	}
	for _, group := range request.Tools {
		if group.Type != "namespace" || group.Name == "" {
			continue
		}
		if allowed[group.Name] == nil {
			allowed[group.Name] = map[string]bool{}
		}
		for _, tool := range group.Tools {
			if tool.Type == "function" && tool.Name != "" {
				allowed[group.Name][tool.Name] = true
			}
		}
	}
	return allowed
}
