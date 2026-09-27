package privacy

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

func withoutMetadata(body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	delete(fields, "metadata")
	return json.Marshal(fields)
}
func hasOnly(n *jsonNode, keys string) bool {
	if n == nil || n.kind != '{' {
		return false
	}
	for _, p := range n.pairs {
		if !strings.Contains(" "+keys+" ", " "+p.key.text+" ") {
			return false
		}
	}
	return true
}
func (e *Engine) checkTransport(body []byte) error {
	n, err := scanJSON(body)
	if !utf8.Valid(body) || err != nil || n.kind != '{' {
		return errTraffic
	}
	if !hasOnly(n, "model system messages tools tool_choice max_tokens stream temperature top_p top_k stop_sequences metadata thinking output_config service_tier") {
		return errTraffic
	}
	if thinking := n.get("thinking"); thinking != nil && thinking.str("type") != "disabled" {
		return errTraffic
	}
	if !transportShape(n, false) {
		return errTraffic
	}
	sources := transportSources(n)
	visited := map[int]bool{}
	_, err = rewriteRequest(body, func(text string, f fieldKind) ([]textEdit, error) {
		visited[f.nodeStart] = true
		if f.system && strings.HasPrefix(text, "x-anthropic-billing-header:") {
			first, _, _ := strings.Cut(text, "\n")
			if e.sensitiveStructural(first) {
				return nil, errTraffic
			}
		}
		return nil, nil
	})
	if err != nil {
		return errTraffic
	}
	var walk func(*jsonNode) bool
	walk = func(v *jsonNode) bool {
		if !visited[v.start] && v.kind != '{' && v.kind != '[' {
			text := v.text
			if v.kind != '"' {
				text = string(body[v.start:v.end])
			}
			if e.sensitiveStructural(text) {
				return false
			}
		}
		if v == n.get("metadata") {
			return true
		} // used locally, removed before transport
		typ := v.str("type")
		if typ == "thinking" || typ == "redacted_thinking" {
			return false
		}
		if sources[v.start] {
			return true
		} // only actual content attachments
		for _, pair := range v.pairs {
			if !walk(pair.key) || !walk(pair.value) {
				return false
			}
		}
		for _, item := range v.items {
			if !walk(item) {
				return false
			}
		}
		return true
	}
	if !walk(n) {
		return errTraffic
	}
	return nil
}

// Structural identifiers cannot be changed without changing tool semantics.
// Reject configured matches, including encoded ones, instead of renaming them.
func (e *Engine) sensitiveStructural(text string) bool {
	if e.detectors.allowed(text) {
		return false
	}
	direct := func(value string) bool {
		spans := append(e.detectors.regex.Detect(value), e.detectors.dict.Detect(value)...)
		if enabled(e.rules.Filters, "patterns") {
			for _, p := range e.rules.patterns {
				spans = append(spans, p.Detect(value)...)
			}
		}
		for _, span := range spans {
			if !e.detectors.allowed(span.Value) {
				return true
			}
		}
		return false
	}
	if direct(text) {
		return true
	}
	for _, token := range encodedTokenRE.FindAllString(text, -1) {
		if len(token) > maxEncodedToken || encodedSensitive(token, maxEncodingDepth, direct) {
			return true
		}
	}
	return false
}

// Full-buffer admission precedes any response bytes. SSE without a terminal
// event, unknown event, upstream error or opaque reasoning is not releasable.
func (x *Exchange) checkTrafficSSE(body []byte) error {
	normalized := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	if !bytes.HasSuffix(normalized, []byte("\n\n")) {
		return errRestore
	}
	stopped := false
	open := map[string]string{}
	seen := map[string]bool{}
	for _, frame := range bytes.Split(normalized, []byte("\n\n")) {
		if len(bytes.TrimSpace(frame)) == 0 {
			continue
		}
		event := ""
		var data []byte
		for _, line := range bytes.Split(frame, []byte("\n")) {
			switch {
			case bytes.HasPrefix(line, []byte("event:")):
				if event != "" {
					return errRestore
				}
				event = strings.TrimSpace(string(line[6:]))
			case bytes.HasPrefix(line, []byte("data:")):
				if data != nil {
					return errRestore
				}
				data = bytes.TrimSpace(line[5:])
			default:
				return errRestore
			}
		}
		n, err := scanJSON(data)
		if err != nil || n.kind != '{' {
			return errRestore
		}
		if event == "" {
			event = n.str("type")
		}
		if event != n.str("type") || stopped {
			return errRestore
		}
		index := n.get("index")
		key := ""
		if index != nil {
			key = string(data[index.start:index.end])
		}
		eventKeys := map[string]string{
			"ping": "type", "message_start": "type message", "message_delta": "type delta usage",
			"content_block_start": "type index content_block", "content_block_delta": "type index delta",
			"content_block_stop": "type index", "message_stop": "type",
		}
		if !hasOnly(n, eventKeys[event]) {
			return errRestore
		}
		switch event {
		case "ping":
			if !hasOnly(n, "type") {
				return errRestore
			}
		case "message_start":
			message := n.get("message")
			if message == nil {
				return errRestore
			}
			if content := message.get("content"); content != nil && (content.kind != '[' || len(content.items) > 0) {
				return errRestore
			}
			if x.checkResponse(data[message.start:message.end]) != nil {
				return errRestore
			}
		case "message_delta":
			delta := n.get("delta")
			if delta == nil || !hasOnly(delta, "stop_reason stop_sequence") || x.checkOpaque(n) != nil {
				return errRestore
			}
		case "content_block_start":
			if key == "" || seen[key] {
				return errRestore
			}
			block := n.get("content_block")
			if block == nil || (block.str("type") != "text" && block.str("type") != "tool_use") {
				return errRestore
			}
			if err := x.checkResponse(append(append([]byte(`{"content":[`), data[block.start:block.end]...), ']', '}')); err != nil {
				return errRestore
			}
			open[key] = block.str("type")
			seen[key] = true
		case "content_block_delta":
			delta := n.get("delta")
			if delta == nil {
				return errRestore
			}
			fields := "type text"
			if open[key] == "tool_use" {
				fields = "type partial_json"
			}
			if !hasOnly(delta, fields) {
				return errRestore
			}
			if delta == nil || open[key] == "" || open[key] == "text" && delta.str("type") != "text_delta" || open[key] == "tool_use" && delta.str("type") != "input_json_delta" {
				return errRestore
			}
		case "content_block_stop":
			if open[key] == "" {
				return errRestore
			}
			delete(open, key)
		case "message_stop":
			if len(open) > 0 {
				return errRestore
			}
			stopped = true
		default:
			return errRestore
		}
	}
	if !stopped {
		return errRestore
	}
	return nil
}

// ValidateObject rejects duplicate keys and invalid UTF-8 before another
// protocol decoder can collapse or repair them.
func ValidateObject(body []byte) error {
	n, err := scanJSON(body)
	if !utf8.Valid(body) || err != nil || n.kind != '{' {
		return errTraffic
	}
	return nil
}
func transportShape(n *jsonNode, response bool) bool {
	if !response {
		for key, allowed := range map[string]string{"output_config": "effort", "tool_choice": "type name disable_parallel_tool_use", "thinking": "type"} {
			if v := n.get(key); v != nil && (v.kind != '{' || !hasOnly(v, allowed)) {
				return false
			}
		}
	}
	var blocks func(*jsonNode) bool
	blocks = func(value *jsonNode) bool {
		if value.kind == '"' {
			return true
		}
		if value.kind != '[' {
			return false
		}
		for _, block := range value.items {
			if block.kind != '{' {
				return false
			}
			keys := ""
			switch block.str("type") {
			case "text":
				keys = "type text cache_control"
			case "tool_use":
				keys = "type id name input cache_control"
				if input := block.get("input"); input == nil || input.kind != '{' {
					return false
				}
			case "tool_result":
				if response {
					return false
				}
				keys = "type tool_use_id content is_error cache_control"
				if content := block.get("content"); content != nil && !blocks(content) {
					return false
				}
			case "image", "document":
				if response {
					return false
				}
				keys = "type source cache_control title context citations"
				if block.get("source") == nil {
					return false
				}
			default:
				return false
			}
			if !hasOnly(block, keys) {
				return false
			}
			if cache := block.get("cache_control"); cache != nil && (!hasOnly(cache, "type ttl") || cache.str("type") != "ephemeral" || cache.str("ttl") != "" && cache.str("ttl") != "5m" && cache.str("ttl") != "1h") {
				return false
			}
		}
		return true
	}
	for _, key := range []string{"system", "content"} {
		if v := n.get(key); v != nil && !blocks(v) {
			return false
		}
	}
	if messages := n.get("messages"); messages != nil {
		if messages.kind != '[' {
			return false
		}
		for _, msg := range messages.items {
			content := msg.get("content")
			if !hasOnly(msg, "role content") || content == nil || !blocks(content) {
				return false
			}
		}
	}
	if tools := n.get("tools"); tools != nil {
		if tools.kind != '[' {
			return false
		}
		for _, tool := range tools.items {
			if !hasOnly(tool, "name description input_schema cache_control") {
				return false
			}
		}
	}
	return true
}
func (x *Exchange) checkResponse(body []byte) error {
	n, err := scanJSON(body)
	if !utf8.Valid(body) || err != nil || n.kind != '{' || !transportShape(n, true) {
		return errRestore
	}
	if !hasOnly(n, "id type role model content stop_reason stop_sequence usage") {
		return errRestore
	}
	visited := map[int]bool{}
	_, err = rewriteResponse(body, func(_ string, f fieldKind) ([]textEdit, error) { visited[f.nodeStart] = true; return nil, nil })
	if err != nil {
		return errRestore
	}
	x.request.mu.Lock()
	defer x.request.mu.Unlock()
	var walk func(*jsonNode) bool
	walk = func(v *jsonNode) bool {
		if v.kind == '"' && !visited[v.start] {
			edits, err := x.request.unmaskText(v.text, fieldKind{})
			if err != nil || len(edits) > 0 {
				return false
			}
		}
		for _, p := range v.pairs {
			if !walk(p.key) || !walk(p.value) {
				return false
			}
		}
		for _, v := range v.items {
			if !walk(v) {
				return false
			}
		}
		return true
	}
	if !walk(n) {
		return errRestore
	}
	return nil
}

// The exception is positional: a JSON schema may legitimately contain a
// property named type; it must never acquire attachment bypass privileges.
func transportSources(root *jsonNode) map[int]bool {
	out := map[int]bool{}
	var content func(*jsonNode)
	content = func(n *jsonNode) {
		if n == nil || n.kind != '[' {
			return
		}
		for _, b := range n.items {
			switch b.str("type") {
			case "image", "document":
				out[b.start] = true
			case "tool_result":
				content(b.get("content"))
			}
		}
	}
	content(root.get("system"))
	content(root.get("content"))
	if messages := root.get("messages"); messages != nil {
		for _, m := range messages.items {
			content(m.get("content"))
		}
	}
	return out
}
func (x *Exchange) CheckControl(value string) error {
	if x == nil {
		return nil
	}
	if x.engine.sensitiveStructural(value) {
		return errTraffic
	}
	x.request.mu.Lock()
	defer x.request.mu.Unlock()
	if x.request.closed {
		return errTraffic
	}
	known := func(text string) bool {
		for _, real := range x.request.spellings {
			if real != "" && strings.Contains(text, real) {
				return true
			}
		}
		for _, real := range x.request.secrets {
			if real != "" && strings.Contains(text, real) {
				return true
			}
		}
		return false
	}
	if known(value) {
		return errTraffic
	}
	for _, token := range encodedTokenRE.FindAllString(value, -1) {
		if encodedSensitive(token, maxEncodingDepth, known) {
			return errTraffic
		}
	}
	return nil
}
func (x *Exchange) checkOpaque(node *jsonNode) error {
	x.request.mu.Lock()
	defer x.request.mu.Unlock()
	var walk func(*jsonNode) bool
	walk = func(n *jsonNode) bool {
		if n.kind == '"' {
			edits, err := x.request.unmaskText(n.text, fieldKind{})
			if err != nil || len(edits) > 0 {
				return false
			}
		}
		for _, p := range n.pairs {
			if !walk(p.key) || !walk(p.value) {
				return false
			}
		}
		for _, v := range n.items {
			if !walk(v) {
				return false
			}
		}
		return true
	}
	if !walk(node) || x.request.stats.Unexpected > 0 {
		return errRestore
	}
	return nil
}
