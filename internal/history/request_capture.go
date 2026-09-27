package history

import (
	"io"
	"sync"
)

// RequestCapture observes only the bytes the upstream consumes. It neither
// pre-reads the body nor changes ContentLength, transfer encoding or Close.
// Transport may read/close asynchronously, so snapshots copy under a lock.
type RequestCapture struct {
	io.ReadCloser
	mu       sync.Mutex
	limit    int
	expected int64
	read     int64
	body     []byte
	eof      bool
	failed   bool
}

func CaptureRequest(body io.ReadCloser, limit int, expected int64) *RequestCapture {
	return &RequestCapture{ReadCloser: body, limit: max(0, limit), expected: expected}
}

func (c *RequestCapture) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.read += int64(n)
	if room := c.limit - len(c.body); room > 0 {
		c.body = append(c.body, p[:min(n, room)]...)
	}
	if err == io.EOF {
		c.eof = true
	} else if err != nil {
		c.failed = true
	}
	return n, err
}

func (c *RequestCapture) Snapshot() ([]byte, bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	incomplete := c.read > int64(len(c.body)) || c.failed || (c.expected >= 0 && c.read < c.expected) || (c.expected < 0 && !c.eof)
	note := ""
	if c.read > int64(len(c.body)) {
		note = "Записано начало тела: достигнут лимит диагностического захвата."
	}
	if c.failed {
		note = "Тело получено не полностью: ошибка чтения."
	} else if incomplete && note == "" {
		note = "Тело было прочитано не полностью до завершения запроса."
	}
	return append([]byte(nil), c.body...), incomplete, note
}
