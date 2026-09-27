package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Stream couples client cancellation to every upstream request, including
// continuations. Result waits for the writer, so callers cannot race state capture.
type Stream struct {
	*io.PipeReader
	cancel     context.CancelFunc
	done       chan struct{}
	completion Completion
	err        error
}

func Start(ctx context.Context, payload []byte, send Sender, options Options, streaming bool) *Stream {
	ctx, cancel := context.WithCancel(ctx)
	r, w := io.Pipe()
	s := &Stream{PipeReader: r, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer cancel()
		var emit func(Delta) error
		var projected strings.Builder
		block := ""
		if streaming {
			emit = func(delta Delta) error {
				if delta.ReasoningContent != "" {
					block = "thinking"
				}
				if delta.Content != "" {
					if block != "text" && projected.Len() > 0 {
						projected.WriteByte('\n')
					}
					projected.WriteString(delta.Content)
					block = "text"
				}
				if len(delta.ToolCalls) > 0 {
					block = "tool"
				}
				return writeChunk(w, map[string]any{"choices": []any{map[string]any{"delta": delta}}})
			}
		}
		s.completion, s.err = Exchange(ctx, payload, send, options, emit)
		if streaming {
			text := projected.String()
			s.completion.VisibleText = &text
		}
		if s.err == nil && options.BeforeFinish != nil {
			s.err = options.BeforeFinish(s.completion)
		}
		if s.err == nil {
			if streaming {
				s.err = finishChat(w, s.completion)
			} else {
				_, s.err = w.Write(s.completion.ChatResponse())
			}
		}
		_ = w.CloseWithError(s.err)
	}()
	return s
}

func (s *Stream) Close() error                { s.cancel(); return s.PipeReader.Close() }
func (s *Stream) Result() (Completion, error) { <-s.done; return s.completion, s.err }

func writeChunk(w io.Writer, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func finishChat(w io.Writer, result Completion) error {
	finish := "stop"
	if len(result.Tools) > 0 {
		finish = "tool_calls"
	}
	// Send usage before the finish marker; an early disconnect must not turn an
	// incomplete stream into success merely because a finish_reason was observed.
	if err := writeChunk(w, map[string]any{"choices": []any{map[string]any{"delta": Delta{}, "finish_reason": finish}}, "usage": result.LastUsage.Chat()}); err != nil {
		return err
	}
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return err
}

// WriteChat translates a single response for callers that already own a body.
// Live routing uses Start, which also owns continuation and cancellation.
func WriteChat(w io.Writer, body io.Reader) error {
	result, err := Read(body, func(delta Delta) error {
		return writeChunk(w, map[string]any{"choices": []any{map[string]any{"delta": delta}}})
	}, nil)
	if err != nil {
		return err
	}
	if result.EndTurn != nil && !*result.EndTurn && len(result.Tools) == 0 {
		return protocolError("continuation_required")
	}
	return finishChat(w, result)
}
