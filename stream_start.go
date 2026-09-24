package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

type responseReader struct {
	io.Reader
	close func() error
}

func (r *responseReader) Close() error { return r.close() }

// Read only the first useful SSE event. Heartbeats and role-only announcements
// cannot disable the response-start timeout. Replay that single event and then
// forward the body incrementally; never collect the whole response.
func firstChatEvent(body io.Reader) (io.Reader, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			return nil, errors.New("stream ended before any response")
		}
		var chunk openaiChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, err
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.Delta.Content == "" && choice.Delta.ReasoningContent == "" && len(choice.Delta.ToolCalls) == 0 && choice.FinishReason == "" {
			continue
		}
		// Keep Scanner as the reader of the remaining lines, including anything it
		// already read ahead, and replay exactly one event to the stream translator.
		return io.MultiReader(strings.NewReader(line+"\n\n"), &scannerReader{scanner: scanner}), nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.ErrUnexpectedEOF
}

type scannerReader struct {
	scanner *bufio.Scanner
	pending bytes.Buffer
}

func (r *scannerReader) Read(p []byte) (int, error) {
	for r.pending.Len() == 0 {
		if !r.scanner.Scan() {
			if err := r.scanner.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		r.pending.Write(r.scanner.Bytes())
		r.pending.WriteByte('\n')
	}
	return r.pending.Read(p)
}
