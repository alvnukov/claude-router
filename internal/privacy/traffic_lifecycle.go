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
		if key == "" || open[key] != "" {
			return false
		}
		block := payload.get("content_block")
		if block != nil && block.kind == '{' && block.str("type") != "" {
			open[key] = block.str("type")
		}
	case "content_block_stop":
		delete(open, key)
	case "content_block_delta":
		if open[key] == "" {
			return false
		}
		delta := payload.get("delta")
		if delta != nil && delta.kind == '{' {
			if field := supportedDeltaField(open[key], delta.str("type")); field != "" {
				if delta.str(field) != "" {
					life.useful()
				}
			} else if open[key] != "text" && open[key] != "tool_use" && open[key] != "thinking" {
				for _, pair := range delta.pairs {
					if pair.key.text != "type" && pair.value.kind == '"' && pair.value.text != "" {
						life.useful()
						break
					}
				}
			}
		}
	case "message_stop":
		return len(open) == 0 && !life.expired.Load()
	}
	return false
}

func nextProtectedFrame(body []byte, start int) ([]byte, int, bool) {
	for pos := start; pos < len(body); {
		end, next, complete := nextSSELine(body, pos)
		if !complete {
			return nil, 0, false
		}
		if end == pos {
			frameStart := start
			if start == 0 && bytes.HasPrefix(body, []byte("\xef\xbb\xbf")) {
				frameStart += 3
			}
			return body[frameStart:pos], next, true
		}
		pos = next
	}
	return nil, 0, false
}

func protectedFrameEvent(frame []byte) (string, *jsonNode, []byte) {
	return protectedParsedFrameEvent(parseSSEFrame(frame))
}

func protectedParsedFrameEvent(parsed parsedSSEFrame) (string, *jsonNode, []byte) {
	event, data := parsed.event, parsed.data
	if len(data) == 0 {
		return "", nil, nil
	}
	n, err := scanJSON(data)
	if err != nil || n.kind != '{' {
		return "", nil, nil
	}
	if event == "" {
		event = n.str("type")
	}
	if event == "" || n.str("type") != "" && n.str("type") != event {
		return "", nil, nil
	}
	switch event {
	case "message_start", "message_delta", "message_stop", "content_block_start", "content_block_delta", "content_block_stop", "error":
		if n.str("type") != event {
			return "", nil, nil
		}
	}
	return event, n, data
}
