package privacy

import (
	"strings"
)

func hasThinking(body []byte) bool {
	n, err := scanJSON(body)
	if err != nil {
		return false
	}
	return visitNodes(n, func(n *jsonNode) bool {
		return n.kind == '{' && (n.str("type") == "thinking" || n.str("type") == "redacted_thinking")
	})
}
func visitNodes(n *jsonNode, f func(*jsonNode) bool) bool {
	if f(n) {
		return true
	}
	for _, p := range n.pairs {
		if visitNodes(p.value, f) {
			return true
		}
	}
	for _, v := range n.items {
		if visitNodes(v, f) {
			return true
		}
	}
	return false
}
func (e *Engine) Check(body []byte) error {
	n, err := scanJSON(body)
	if err != nil {
		return err
	}
	if e.opt.supportedOnly && ValidateObject(body) != nil {
		return errTraffic
	}
	if tools := n.get("tools"); tools != nil && !e.opt.supportedOnly {
		for _, tool := range tools.items {
			name := tool.str("name")
			if strings.HasPrefix(name, "mcp__claude_ai_") {
				return &RejectError{Reason: "disable connectors: ENABLE_CLAUDEAI_MCP_SERVERS=false", Path: "tools.name"}
			}
			if strings.HasPrefix(name, "Artifact") {
				return &RejectError{Reason: "disable artifacts: CLAUDE_CODE_DISABLE_ARTIFACT=1", Path: "tools.name"}
			}
			if e.dictionaryCollision(name) {
				return &RejectError{Reason: "dictionary collides with tool name", Path: "tools.name"}
			}
			if schema := tool.get("input_schema"); schema != nil {
				var collision string
				visitNodes(schema, func(v *jsonNode) bool {
					if p := v.get("properties"); p != nil {
						for _, field := range p.pairs {
							if e.dictionaryCollision(field.key.text) {
								collision = field.key.text
								return true
							}
						}
					}
					return false
				})
				if collision != "" {
					return &RejectError{Reason: "dictionary collides with schema property", Path: "tools.input_schema.properties"}
				}
			}
		}
	}
	if !e.opt.supportedOnly && visitNodes(n, func(v *jsonNode) bool {
		if v.str("type") == "thinking" {
			for _, s := range e.detectors.dict.Detect(v.str("thinking")) {
				if !e.detectors.allowed(s.Value) {
					return true
				}
			}
		}
		return false
	}) {
		return &RejectError{Reason: "thinking содержит реальные значения; начните новую сессию под профилем"}
	}
	_, err = rewriteRecordedMode(body, func(text string, f fieldKind) ([]textEdit, error) {
		if e.allowedPath(f.path) {
			return nil, nil
		}
		for _, s := range e.detectors.regex.Detect(text) {
			if e.detectors.allowed(s.Value) {
				continue
			}
			if a, _, ok := networkSpan(s.Value); ok {
				if e.rules.PublicRange.Contains(a) || e.rules.PublicRange6.Contains(a) {
					return nil, (&ipMapper{rules: e.rules}).checkInput(a)
				}
			}
		}
		return nil, nil
	}, nil, nil, e.opt.supportedOnly)
	return err
}
func (e *Engine) dictionaryCollision(s string) bool {
	if e.detectors.allowed(s) {
		return false
	}
	for _, span := range e.detectors.dict.Detect(s) {
		if span.Start == 0 && span.End == len(s) {
			return true
		}
	}
	return false
}
