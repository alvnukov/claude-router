package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// A record is one /v1/messages call as the router saw it: the Anthropic-shaped
// request that came in, what was sent on (translated, for the local path), and
// the Anthropic-shaped response that went back. Both paths answer in the
// Anthropic wire format, so one parser covers cloud and local alike.
type record struct {
	ID      string
	Seq     int64
	Start   time.Time
	End     time.Time
	Path    string
	Model   string
	Route   string // "cloud" | "local"
	Session string // Claude Code session id from metadata.user_id, "" when absent
	Stream  bool
	Headers map[string]string
	ReqBody []byte

	// Local path only.
	OpenAIBody []byte
	TrimBefore int
	TrimAfter  int
	TrimNotes  []string
	Served     string
	Attempts   []attempt

	Status        int
	RespCT        string
	RespBytes     []byte
	RespTruncated bool
	Resp          *parsedResponse
}

func (r *record) Done() bool { return !r.End.IsZero() }

// sessionOf pulls the Claude Code session id out of a request. The CLI sends
// metadata.user_id as a JSON string holding device_id, account_uuid and
// session_id; other clients leave it out and the record stays unsessioned.
func sessionOf(body []byte) string {
	var req struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &req) != nil || req.Metadata.UserID == "" {
		return ""
	}
	var uid struct {
		SessionID string `json:"session_id"`
	}
	json.Unmarshal([]byte(req.Metadata.UserID), &uid)
	return uid.SessionID
}

func (r *record) Duration() time.Duration {
	if r.End.IsZero() {
		return time.Since(r.Start)
	}
	return r.End.Sub(r.Start)
}

func (r *record) Failed() bool {
	return r.Done() && (r.Status >= 400 || (r.Resp != nil && r.Resp.Error != ""))
}

// localTrace is what handleLocal reports back about the translated request. It
// is written before the response starts and read only after the handler
// returns, so it needs no lock.
type localTrace struct {
	OpenAIBody []byte
	TrimBefore int
	TrimAfter  int
	TrimNotes  []string
	Served     string    // model that produced the response
	Attempts   []attempt // every model tried, in order
}

type attempt struct {
	Model string
	Err   string // empty on success
	Dur   time.Duration
}

type respBlock struct {
	Type  string
	Text  string
	ID    string
	Name  string
	Input string
}

type parsedResponse struct {
	Blocks     []respBlock
	StopReason string
	Usage      map[string]int
	Model      string
	Error      string // an error the API returned
	Note       string // a problem with the capture itself
	Events     int
}

func (p *parsedResponse) Text() string {
	var b strings.Builder
	for _, blk := range p.Blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// store is a fixed-size ring of records, newest last. Reads hand out shallow
// copies: byte slices are never mutated once attached, so sharing them is safe.
type store struct {
	mu   sync.RWMutex
	max  int
	seq  int64
	recs []*record

	path  string // history file; "" keeps history in memory only
	life  *lifecycle
	fmu   sync.Mutex // serialises file writes
	lines int        // lines currently in the file, for compaction
}

func newStore(max int, path string) *store {
	if max < 1 {
		max = 1
	}
	s := &store{max: max, path: path}
	s.load()
	return s
}

func (s *store) add(r *record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	r.Seq = s.seq
	r.ID = newID("req_")
	s.recs = append(s.recs, r)
	if len(s.recs) > s.max {
		drop := len(s.recs) - s.max
		copy(s.recs, s.recs[drop:])
		for i := len(s.recs) - drop; i < len(s.recs); i++ {
			s.recs[i] = nil
		}
		s.recs = s.recs[:len(s.recs)-drop]
	}
}

func (s *store) update(id string, fn func(*record)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.recs {
		if r.ID == id {
			fn(r)
			return
		}
	}
}

func (s *store) get(id string) *record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.recs {
		if r.ID == id {
			c := *r
			return &c
		}
	}
	return nil
}

// list returns copies, newest first.
func (s *store) list() []*record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*record, 0, len(s.recs))
	for i := len(s.recs) - 1; i >= 0; i-- {
		c := *s.recs[i]
		out = append(out, &c)
	}
	return out
}

func (s *store) clear() {
	s.mu.Lock()
	s.recs = nil
	s.mu.Unlock()
	s.truncate()
}

func (s *store) size() (int, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.recs), s.max
}

// recorder tees the response into a bounded buffer. Flush and Unwrap keep both
// streaming paths working: the local SSE writer type-asserts http.Flusher, the
// reverse proxy reaches the real writer through Unwrap.
type recorder struct {
	http.ResponseWriter
	status    int
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func newRecorder(w http.ResponseWriter, limit int) *recorder {
	return &recorder{ResponseWriter: w, limit: limit}
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if room := r.limit - r.buf.Len(); room > 0 {
		if len(b) > room {
			r.buf.Write(b[:room])
			r.truncated = true
		} else {
			r.buf.Write(b)
		}
	} else if len(b) > 0 {
		r.truncated = true
	}
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// finish attaches the response and the local trace to the record.
func (s *store) finish(id string, rw *recorder, tr *localTrace) {
	ct := rw.Header().Get("Content-Type")
	body := rw.buf.Bytes()
	parsed := parseResponse(ct, rw.Header().Get("Content-Encoding"), body, rw.truncated)
	s.update(id, func(r *record) {
		r.End = time.Now()
		r.Status = rw.status
		r.RespCT = ct
		r.RespBytes = body
		r.RespTruncated = rw.truncated
		r.Resp = parsed
		if tr != nil {
			r.OpenAIBody = tr.OpenAIBody
			r.TrimBefore = tr.TrimBefore
			r.TrimAfter = tr.TrimAfter
			r.TrimNotes = tr.TrimNotes
			r.Served = tr.Served
			r.Attempts = tr.Attempts
		}
	})
	if r := s.get(id); r != nil {
		s.persist(r)
	}
}

func pickHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for _, k := range []string{"User-Agent", "Anthropic-Beta", "Anthropic-Version", "X-App", "Content-Length"} {
		if v := h.Get(k); v != "" {
			out[k] = v
		}
	}
	return out
}

// ---- Anthropic response parsing ----

func parseResponse(ct, enc string, body []byte, truncated bool) *parsedResponse {
	body, note := decodeBody(enc, body)
	var p *parsedResponse
	if strings.Contains(ct, "text/event-stream") {
		p = parseSSE(body)
	} else {
		p = parseJSONResponse(body, truncated)
	}
	if note != "" {
		p.Note = note
	} else if truncated && p.Note == "" {
		p.Note = "ответ обрезан при захвате (лимит буфера)"
	}
	return p
}

// decodeBody undoes gzip when a response still arrived compressed. Anything
// else (br, zstd) is left alone and reported: the client got it fine, only the
// capture cannot read it.
func decodeBody(enc string, body []byte) ([]byte, string) {
	isGzip := len(body) > 2 && body[0] == 0x1f && body[1] == 0x8b
	switch {
	case isGzip || strings.Contains(enc, "gzip"):
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return body, "gzip: " + err.Error()
		}
		out, err := io.ReadAll(zr)
		if err != nil && len(out) == 0 {
			return body, "gzip: " + err.Error()
		}
		return out, ""
	case enc != "" && enc != "identity":
		return body, "ответ сжат (" + enc + "), захват не декодирует этот формат"
	}
	return body, ""
}

func parseJSONResponse(body []byte, truncated bool) *parsedResponse {
	p := &parsedResponse{Usage: map[string]int{}}
	if len(bytes.TrimSpace(body)) == 0 {
		return p
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		if truncated {
			p.Note = "ответ обрезан при захвате (лимит буфера)"
		} else {
			p.Note = "тело ответа не JSON: " + err.Error()
		}
		return p
	}
	if kind, _ := jsonString(m["type"]); kind == "error" {
		var e struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		json.Unmarshal(m["error"], &e)
		p.Error = strings.TrimSpace(e.Type + ": " + e.Message)
		return p
	}
	p.Model, _ = jsonString(m["model"])
	p.StopReason, _ = jsonString(m["stop_reason"])
	p.Usage = usageMap(m["usage"])
	var blocks []map[string]json.RawMessage
	json.Unmarshal(m["content"], &blocks)
	for _, b := range blocks {
		rb := respBlock{}
		rb.Type, _ = jsonString(b["type"])
		rb.ID, _ = jsonString(b["id"])
		rb.Name, _ = jsonString(b["name"])
		switch rb.Type {
		case "text":
			rb.Text, _ = jsonString(b["text"])
		case "thinking":
			rb.Text, _ = jsonString(b["thinking"])
		case "tool_use":
			rb.Input = prettyJSON(b["input"])
		default:
			rb.Text = prettyJSON(mustMarshal(b))
		}
		p.Blocks = append(p.Blocks, rb)
	}
	return p
}

func parseSSE(body []byte) *parsedResponse {
	p := &parsedResponse{Usage: map[string]int{}}
	type open struct {
		blk   respBlock
		input strings.Builder
	}
	blocks := map[int]*open{}
	order := []int{}

	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var data strings.Builder
	flush := func() {
		if data.Len() == 0 {
			return
		}
		raw := data.String()
		data.Reset()
		var ev map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &ev) != nil {
			return
		}
		p.Events++
		kind, _ := jsonString(ev["type"])
		switch kind {
		case "message_start":
			var msg map[string]json.RawMessage
			json.Unmarshal(ev["message"], &msg)
			p.Model, _ = jsonString(msg["model"])
			for k, v := range usageMap(msg["usage"]) {
				p.Usage[k] = v
			}
		case "content_block_start":
			idx := jsonInt(ev["index"])
			var cb map[string]json.RawMessage
			json.Unmarshal(ev["content_block"], &cb)
			o := &open{}
			o.blk.Type, _ = jsonString(cb["type"])
			o.blk.ID, _ = jsonString(cb["id"])
			o.blk.Name, _ = jsonString(cb["name"])
			o.blk.Text, _ = jsonString(cb["text"])
			if o.blk.Type == "thinking" {
				o.blk.Text, _ = jsonString(cb["thinking"])
			}
			if _, seen := blocks[idx]; !seen {
				order = append(order, idx)
			}
			blocks[idx] = o
		case "content_block_delta":
			idx := jsonInt(ev["index"])
			o, ok := blocks[idx]
			if !ok {
				o = &open{blk: respBlock{Type: "text"}}
				blocks[idx] = o
				order = append(order, idx)
			}
			var d map[string]json.RawMessage
			json.Unmarshal(ev["delta"], &d)
			dt, _ := jsonString(d["type"])
			switch dt {
			case "text_delta":
				s, _ := jsonString(d["text"])
				o.blk.Text += s
			case "thinking_delta":
				s, _ := jsonString(d["thinking"])
				o.blk.Text += s
			case "input_json_delta":
				s, _ := jsonString(d["partial_json"])
				o.input.WriteString(s)
			}
		case "message_delta":
			var d map[string]json.RawMessage
			json.Unmarshal(ev["delta"], &d)
			if sr, ok := jsonString(d["stop_reason"]); ok && sr != "" {
				p.StopReason = sr
			}
			for k, v := range usageMap(ev["usage"]) {
				p.Usage[k] = v
			}
		case "error":
			var e struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}
			json.Unmarshal(ev["error"], &e)
			p.Error = strings.TrimSpace(e.Type + ": " + e.Message)
		}
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	flush()
	for _, idx := range order {
		o := blocks[idx]
		if o.input.Len() > 0 {
			o.blk.Input = prettyJSON(json.RawMessage(o.input.String()))
		}
		p.Blocks = append(p.Blocks, o.blk)
	}
	return p
}

func jsonString(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func jsonInt(raw json.RawMessage) int {
	var n int
	json.Unmarshal(raw, &n)
	return n
}

func usageMap(raw json.RawMessage) map[string]int {
	out := map[string]int{}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return out
	}
	for k, v := range m {
		if f, ok := v.(float64); ok {
			out[k] = int(f)
		}
	}
	return out
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// prettyJSON re-indents valid JSON and passes anything else through untouched.
func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var out bytes.Buffer
	if json.Indent(&out, raw, "", "  ") != nil {
		return string(raw)
	}
	return out.String()
}
