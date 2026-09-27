package privacy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type wallLifecycleClock struct{}

func (wallLifecycleClock) Now() time.Time { return time.Now() }
func (wallLifecycleClock) AfterFunc(d time.Duration, fn func()) LifecycleTimer {
	return time.AfterFunc(d, fn)
}

type protectedLifecycle struct {
	clock       LifecycleClock
	limits      LifecycleLimits
	controller  *http.ResponseController
	cancel      context.CancelFunc
	stopParent  func() bool
	mu          sync.Mutex
	committed   bool
	finished    bool
	headersSeen bool
	body        io.ReadCloser
	headers     LifecycleTimer
	idle        LifecycleTimer
	total       LifecycleTimer
	expired     atomic.Bool
}

func (life *protectedLifecycle) begin(parent context.Context, cancel context.CancelFunc) {
	life.cancel = cancel
	life.total = life.clock.AfterFunc(life.limits.Total, life.abort)
	life.headers = life.clock.AfterFunc(life.limits.Headers, life.abort)
	life.stopParent = context.AfterFunc(parent, life.abort)
}

func (life *protectedLifecycle) abort() {
	life.mu.Lock()
	if life.finished || life.expired.Load() {
		life.mu.Unlock()
		return
	}
	life.expired.Store(true)
	body := life.body
	deadline := time.Now()
	if !life.committed {
		deadline = deadline.Add(life.limits.Cleanup)
	}
	_ = life.controller.SetWriteDeadline(deadline)
	life.mu.Unlock()
	life.cancel()
	if body != nil {
		_ = body.Close()
	}
}

func (life *protectedLifecycle) commit(ctx context.Context) bool {
	life.mu.Lock()
	defer life.mu.Unlock()
	if life.expired.Load() || ctx.Err() != nil {
		return false
	}
	life.committed = true
	return true
}

func (life *protectedLifecycle) gotHeaders(body io.ReadCloser) {
	life.mu.Lock()
	if life.finished || life.expired.Load() {
		life.mu.Unlock()
		if body != nil {
			_ = body.Close()
		}
		return
	}
	if body != nil {
		life.body = body
	}
	if life.headersSeen {
		life.mu.Unlock()
		return
	}
	life.headersSeen = true
	life.headers.Stop()
	if life.idle != nil {
		life.idle.Stop()
	}
	life.idle = life.clock.AfterFunc(life.limits.Idle, life.abort)
	life.mu.Unlock()
}

func (life *protectedLifecycle) useful() {
	life.mu.Lock()
	defer life.mu.Unlock()
	if life.idle != nil {
		life.idle.Stop()
	}
	life.idle = life.clock.AfterFunc(life.limits.Idle, life.abort)
}

func (life *protectedLifecycle) stop() {
	life.mu.Lock()
	life.finished = true
	body := life.body
	life.mu.Unlock()
	if life.stopParent != nil {
		life.stopParent()
	}
	if body != nil {
		_ = body.Close()
	}
	if life.headers != nil {
		life.headers.Stop()
	}
	life.mu.Lock()
	if life.idle != nil {
		life.idle.Stop()
	}
	life.mu.Unlock()
	if life.total != nil {
		life.total.Stop()
	}
	if life.cancel != nil {
		life.cancel()
	}
}

func readProtectedBody(body io.Reader, stream bool, life *protectedLifecycle) ([]byte, error) {
	var result []byte
	chunk := make([]byte, 32<<10)
	processed := 0
	open := map[string]string{}
	for {
		n, err := body.Read(chunk)
		if n > 0 {
			remaining := TrafficOutputLimit - len(result)
			overLimit := n > remaining
			if overLimit {
				n = remaining
			}
			result = append(result, chunk[:n]...)
			if !stream {
				life.useful()
			} else {
				for {
					frame, end, complete := nextProtectedFrame(result, processed)
					if !complete {
						break
					}
					processed = end
					if observeProtectedFrame(frame, open, life) {
						return result[:end], nil
					}
				}
			}
			if overLimit {
				return nil, errors.New("privacy: response limit")
			}
		}
		if life.expired.Load() {
			return nil, errors.New("privacy: response deadline")
		}
		if err == io.EOF {
			if stream {
				return nil, errors.New("privacy: incomplete stream")
			}
			return result, nil
		}
		if err != nil {
			return nil, errors.New("privacy: response read failed")
		}
	}
}

func observeProtectedFrame(frame []byte, open map[string]string, life *protectedLifecycle) bool {
	event, payload, data := protectedFrameEvent(frame)
	if payload == nil {
		return false
	}
	index := payload.get("index")
	key := ""
	if index != nil {
		key = string(data[index.start:index.end])
	}
	switch event {
	case "content_block_start":
		if !hasOnly(payload, "type index content_block") || key == "" || open[key] != "" {
			return false
		}
		block := payload.get("content_block")
		if block != nil && (block.str("type") == "text" && hasOnly(block, "type text") || block.str("type") == "tool_use" && hasOnly(block, "type id name input")) {
			open[key] = block.str("type")
		}
	case "content_block_stop":
		if hasOnly(payload, "type index") {
			delete(open, key)
		}
	case "content_block_delta":
		if !hasOnly(payload, "type index delta") {
			return false
		}
		delta := payload.get("delta")
		if delta != nil && (open[key] == "text" && hasOnly(delta, "type text") && delta.str("type") == "text_delta" && delta.str("text") != "" || open[key] == "tool_use" && hasOnly(delta, "type partial_json") && delta.str("type") == "input_json_delta" && delta.str("partial_json") != "") {
			life.useful()
		}
	case "message_stop":
		return hasOnly(payload, "type") && len(open) == 0 && !life.expired.Load()
	}
	return false
}

func nextProtectedFrame(body []byte, start int) ([]byte, int, bool) {
	plain := bytes.Index(body[start:], []byte("\n\n"))
	crlf := bytes.Index(body[start:], []byte("\r\n\r\n"))
	if plain < 0 && crlf < 0 {
		return nil, 0, false
	}
	if crlf >= 0 && (plain < 0 || crlf < plain) {
		return body[start : start+crlf], start + crlf + 4, true
	}
	return body[start : start+plain], start + plain + 2, true
}

func protectedFrameEvent(frame []byte) (string, *jsonNode, []byte) {
	frame = bytes.ReplaceAll(frame, []byte("\r\n"), []byte("\n"))
	var event string
	var data []byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		switch {
		case bytes.HasPrefix(line, []byte("event:")) && event == "":
			event = string(bytes.TrimSpace(line[6:]))
		case bytes.HasPrefix(line, []byte("data:")) && data == nil:
			data = bytes.TrimSpace(line[5:])
		default:
			return "", nil, nil
		}
	}
	n, err := scanJSON(data)
	if err != nil || n.kind != '{' || n.str("type") != event {
		return "", nil, nil
	}
	return event, n, data
}
