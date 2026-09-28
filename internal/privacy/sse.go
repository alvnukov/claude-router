package privacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

type blockTail struct{ text, typ string }

type sseDataPart struct{ start, end, offset int }
type parsedSSEFrame struct {
	event   string
	data    []byte
	parts   []sseDataPart
	hasData bool
}

func nextSSELine(body []byte, start int) (int, int, bool) {
	end := bytes.IndexAny(body[start:], "\r\n")
	if end < 0 {
		return len(body), len(body), false
	}
	end += start
	next := end + 1
	if body[end] == '\r' && next < len(body) && body[next] == '\n' {
		next++
	}
	return end, next, true
}

// Decode the SSE envelope while retaining raw offsets for selective JSON edits.
// Comments and service fields are carried through verbatim, and repeated data
// fields are joined with LF exactly as required by the event-stream grammar.
func parseSSEFrame(frame []byte) parsedSSEFrame {
	return parseSSEFrameAt(frame, 0, false)
}

func parseSSEFrameAt(frame []byte, start int, keepOffsets bool) parsedSSEFrame {
	var parsed parsedSSEFrame
	ownedData := false
	for pos := start; pos < len(frame); {
		end, next, _ := nextSSELine(frame, pos)
		start := pos
		line := frame[start:end]
		colon := bytes.IndexByte(line, ':')
		field, valueStart := line, end
		if colon >= 0 {
			field, valueStart = line[:colon], start+colon+1
			if valueStart < end && frame[valueStart] == ' ' {
				valueStart++
			}
		}
		switch string(field) {
		case "event":
			parsed.event = string(frame[valueStart:end])
		case "data":
			if parsed.hasData {
				if !ownedData {
					data := make([]byte, len(parsed.data), len(frame))
					copy(data, parsed.data)
					parsed.data, ownedData = data, true
				}
				parsed.data = append(parsed.data, '\n')
			}
			if keepOffsets && valueStart < end {
				parsed.parts = append(parsed.parts, sseDataPart{valueStart, end, len(parsed.data)})
			}
			if parsed.hasData {
				parsed.data = append(parsed.data, frame[valueStart:end]...)
			} else {
				parsed.data = frame[valueStart:end]
			}
			parsed.hasData = true
		}
		pos = next
	}
	return parsed
}

func (s *streamUnmasker) emitPayloadEdits(frame []byte, parsed parsedSSEFrame, edits []rawEdit) error {
	raw := make([]rawEdit, 0, len(edits))
	for _, edit := range edits {
		found := false
		for _, part := range parsed.parts {
			if edit.start >= part.offset && edit.end <= part.offset+part.end-part.start {
				raw = append(raw, rawEdit{part.start + edit.start - part.offset, part.start + edit.end - part.offset, edit.data})
				found = true
				break
			}
		}
		if !found {
			return errors.New("SSE JSON edit crosses data lines")
		}
	}
	return s.emit(applyRaw(frame, raw))
}

type streamUnmasker struct {
	req        *Request
	out        io.Writer
	buf        []byte
	blocks     map[int]blockTail
	kinds      map[int]string
	err        error
	closed     bool
	framesSeen bool
}

func (e *Engine) NewStreamUnmasker(req *Request, w io.Writer) io.WriteCloser {
	s := &streamUnmasker{req: req, out: w, blocks: make(map[int]blockTail), kinds: make(map[int]string)}
	if req == nil || req.engine != e {
		s.err = errors.New("privacy: foreign or missing request")
	} else {
		req.mu.Lock()
		if req.closed {
			s.err = errors.New("privacy: request dictionary is closed")
		}
		req.mu.Unlock()
	}
	return s
}
func (s *streamUnmasker) Write(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("privacy: stream closed")
	}
	if s.err != nil {
		return 0, s.err
	}
	s.buf = append(s.buf, p...)
	for {
		if s.req.engine.opt.supportedOnly {
			_, end, complete := nextProtectedFrame(s.buf, 0)
			if !complete {
				if len(s.buf) > 16<<20 {
					return 0, s.fail(errors.New("SSE frame exceeds 16 MiB"))
				}
				break
			}
			if end > 16<<20 {
				return 0, s.fail(errors.New("SSE frame exceeds 16 MiB"))
			}
			if err := s.frame(s.buf[:end]); err != nil {
				return 0, s.fail(err)
			}
			s.buf = s.buf[end:]
			continue
		}
		a, n := bytes.Index(s.buf, []byte("\n\n")), 2
		if b := bytes.Index(s.buf, []byte("\r\n\r\n")); b >= 0 && (a < 0 || b < a) {
			a, n = b, 4
		}
		if a < 0 {
			if len(s.buf) > 16<<20 {
				return 0, s.fail(errors.New("SSE frame exceeds 16 MiB"))
			}
			break
		}
		if a+n > 16<<20 {
			return 0, s.fail(errors.New("SSE frame exceeds 16 MiB"))
		}
		frame := s.buf[:a+n]
		if err := s.frame(frame); err != nil {
			return 0, s.fail(err)
		}
		s.buf = s.buf[a+n:]
	}
	if len(s.buf) == 0 {
		s.buf = nil
	}
	return len(p), nil
}
func (s *streamUnmasker) fail(err error) error {
	if s.err != nil {
		return s.err
	}
	s.err = err
	s.buf = nil
	clear(s.blocks)
	clear(s.kinds)
	message, _ := json.Marshal("privacy: unmask: " + err.Error())
	_, _ = fmt.Fprintf(s.out, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":%s}}\n\n", message)
	return err
}
func (s *streamUnmasker) emit(data []byte) error {
	n, err := s.out.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}
func (s *streamUnmasker) frame(frame []byte) error {
	event := ""
	var payload []byte
	var parsed parsedSSEFrame
	start, end := -1, -1
	if s.req.engine.opt.supportedOnly {
		offset := 0
		if !s.framesSeen && bytes.HasPrefix(frame, []byte("\xef\xbb\xbf")) {
			offset = 3
		}
		s.framesSeen = true
		parsed = parseSSEFrameAt(frame, offset, true)
		event, payload = parsed.event, parsed.data
		if !parsed.hasData {
			return s.emit(frame)
		}
		start = 0 // selective parsing uses the offset table instead of a single span
	} else {
		for pos := 0; pos < len(frame); {
			i := bytes.IndexByte(frame[pos:], '\n')
			if i < 0 {
				break
			}
			i += pos
			line := bytes.TrimSuffix(frame[pos:i], []byte{'\r'})
			if bytes.HasPrefix(line, []byte("event:")) {
				event = strings.TrimSpace(string(line[6:]))
			}
			if bytes.HasPrefix(line, []byte("data:")) {
				if start >= 0 {
					return errors.New("multiple SSE data lines are unsupported")
				}
				a := pos + 5
				if a < i && frame[a] == ' ' {
					a++
				}
				start, end = a, pos+len(line)
				payload = frame[start:end]
			}
			pos = i + 1
		}
	}
	if !s.req.engine.opt.supportedOnly && start >= 0 {
		parsed.parts = []sseDataPart{{start, end, 0}}
	}
	// A missing event field uses data.type, as permitted by SSE.
	if event == "" && len(payload) > 0 {
		if n, err := scanJSON(payload); err == nil {
			event = n.str("type")
		}
	}
	// Unknown/control frames are opaque, including upstream idle/error events.
	switch event {
	case "content_block_delta", "content_block_start", "content_block_stop", "message_stop":
	default:
		return s.emit(frame)
	}
	if start < 0 {
		return errors.New("missing SSE data")
	}
	node, err := scanJSON(payload)
	if err != nil {
		return errors.New("invalid SSE data")
	}
	if node.kind != '{' || node.str("type") != "" && node.str("type") != event {
		return errors.New("inconsistent SSE event type")
	}
	index := 0
	if n := node.get("index"); n != nil {
		index, err = strconv.Atoi(string(payload[n.start:n.end]))
		if err != nil || index < 0 {
			return errors.New("invalid SSE block index")
		}
	}
	switch event {
	case "content_block_stop":
		delete(s.kinds, index)
		if err := s.flush(index); err != nil {
			return err
		}
		return s.emit(frame)
	case "message_stop":
		if err := s.flushAll(); err != nil {
			return err
		}
		return s.emit(frame)
	case "content_block_start":
		block := node.get("content_block")
		if block == nil {
			return errors.New("missing SSE content block")
		}
		s.kinds[index] = block.str("type")
		if s.req.engine.opt.supportedOnly && block.str("type") != "text" && block.str("type") != "tool_use" {
			return s.emit(frame)
		}
		if !s.req.engine.opt.supportedOnly && block.str("type") == "tool_use" {
			input := block.get("input")
			if input != nil && (input.kind != '{' || len(input.pairs) > 0) {
				return errors.New("non-empty SSE initial tool input is unsupported")
			}
		}
		s.req.mu.Lock()
		w := jsonRewriter{body: payload, f: s.req.unmaskText, supportedOnly: s.req.engine.opt.supportedOnly}
		if block.str("type") == "text" {
			w.f = func(text string, field fieldKind) ([]textEdit, error) {
				cut := s.holdFrom(text)
				edits, err := s.req.unmaskText(text[:cut], field)
				if err != nil {
					return nil, err
				}
				s.blocks[index] = blockTail{text: text[cut:], typ: "text_delta"}
				if cut < len(text) {
					edits = append(edits, textEdit{cut, len(text), "", nil})
				}
				return edits, nil
			}
		}
		err = w.block(block, fmt.Sprintf("content[%d]", index), false)
		s.req.mu.Unlock()
		if err != nil {
			return err
		}
		if len(w.edits) == 0 {
			return s.emit(frame)
		}
		return s.emitPayloadEdits(frame, parsed, w.edits)
	case "content_block_delta":
		delta := node.get("delta")
		if delta == nil {
			return errors.New("missing SSE delta")
		}
		typ := delta.str("type")
		if s.req.engine.opt.supportedOnly && !((s.kinds[index] == "text" && typ == "text_delta") || (s.kinds[index] == "tool_use" && typ == "input_json_delta")) {
			return s.emit(frame)
		}
		field := "text"
		if typ == "input_json_delta" {
			field = "partial_json"
		} else if typ != "text_delta" {
			return s.emit(frame)
		}
		value := delta.get(field)
		if value == nil || value.kind != '"' {
			return errors.New("invalid SSE delta text")
		}
		tail := s.blocks[index]
		if tail.typ != "" && tail.typ != typ {
			return errors.New("SSE delta kind changed within block")
		}
		text := tail.text + value.text
		if len(text) > 16<<20 {
			return errors.New("SSE block exceeds 16 MiB")
		}
		if typ == "input_json_delta" {
			s.blocks[index] = blockTail{text: text, typ: typ}
			return s.emitPayloadEdits(frame, parsed, []rawEdit{{value.start, value.end, []byte(`""`)}})
		}
		s.req.mu.Lock()
		cut := s.holdFrom(text)
		// An exact match can still grow into a longer key in the next delta.
		edits, err := s.req.unmaskText(text[:cut], fieldKind{})
		s.req.mu.Unlock()
		if err != nil {
			return err
		}
		restored := applyText(text[:cut], edits)
		s.blocks[index] = blockTail{text: text[cut:], typ: typ}
		encoded, _ := json.Marshal(restored)
		return s.emitPayloadEdits(frame, parsed, []rawEdit{{value.start, value.end, encoded}})
	}
	return nil
}
func applyText(text string, edits []textEdit) string {
	if len(edits) == 0 {
		return text
	}
	var b strings.Builder
	pos := 0
	for _, e := range edits {
		b.WriteString(text[pos:e.start])
		b.WriteString(e.value)
		pos = e.end
	}
	b.WriteString(text[pos:])
	return b.String()
}
func (s *streamUnmasker) holdFrom(text string) int {
	cut := s.req.undo.suffixPrefix(text)
	// Unknown placeholders must be counted once, even when split in transit.
	if i := strings.LastIndex(text, "<"); i >= 0 {
		tail := text[i:]
		if strings.HasPrefix("<secret:", tail) || strings.HasPrefix(tail, "<secret:") && !strings.Contains(tail, ">") {
			cut = min(cut, i)
		}
	}
	// An address cannot be decided until its delimiter arrives. Keep just the
	// candidate suffix; holding text never requires retaining an upstream frame.
	start := len(text)
	for start > 0 {
		b := text[start-1]
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '.' || b == ':' || b == '-' || b == '/' || b == '%' {
			start--
		} else {
			break
		}
	}
	if start < len(text) {
		cut = min(cut, start)
	}
	// Do not split a complete mapped value because its final digits happen to
	// look like the beginning of an IP literal.
	for _, m := range s.req.undo.matches(text) {
		if m.start < cut && m.end > cut {
			cut = m.start
		}
	}
	for cut > 0 && cut < len(text) && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return cut
}
func (s *streamUnmasker) flush(index int) error {
	tail, ok := s.blocks[index]
	if !ok {
		return nil
	}
	delete(s.blocks, index)
	if tail.text == "" {
		return nil
	}
	s.req.mu.Lock()
	var restored string
	var err error
	if tail.typ == "input_json_delta" {
		var node *jsonNode
		node, err = scanJSON([]byte(tail.text))
		if err == nil && node.kind != '{' {
			err = correctionError()
		}
		if err == nil {
			w := jsonRewriter{body: []byte(tail.text), f: s.req.unmaskText, supportedOnly: s.req.engine.opt.supportedOnly}
			err = w.allStrings(node, fmt.Sprintf("content[%d].input", index), true)
			if err == nil {
				restored = string(applyRaw(w.body, w.edits))
				_, err = scanJSON([]byte(restored))
			}
		}
	} else {
		var edits []textEdit
		edits, err = s.req.unmaskText(tail.text, fieldKind{})
		if err == nil {
			restored = applyText(tail.text, edits)
		}
	}
	s.req.mu.Unlock()
	if err != nil {
		return err
	}
	value, _ := json.Marshal(restored)
	field := "text"
	if tail.typ == "input_json_delta" {
		field = "partial_json"
	}
	return s.emit(fmt.Appendf(nil, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":%q,%q:%s}}\n\n", index, tail.typ, field, value))
}
func (s *streamUnmasker) flushAll() error {
	keys := make([]int, 0, len(s.blocks))
	for index := range s.blocks {
		keys = append(keys, index)
	}
	sort.Ints(keys)
	for _, index := range keys {
		if err := s.flush(index); err != nil {
			return err
		}
	}
	return nil
}
func (s *streamUnmasker) Close() error {
	if s.closed {
		return s.err
	}
	s.closed = true
	if s.err != nil {
		return s.err
	}
	if len(bytes.TrimSpace(s.buf)) > 0 {
		return s.fail(errors.New("incomplete SSE frame"))
	}
	if err := s.flushAll(); err != nil {
		return s.fail(err)
	}
	return nil
}
