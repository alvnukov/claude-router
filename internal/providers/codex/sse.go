package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

// events implements SSE framing, including multiline data, comments, CRLF and
// a last event without a trailing blank line. A terminal event stops reading.
func events(r io.Reader, consume func(wireEvent) (bool, error)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxProtocolBytes)
	var data bytes.Buffer
	dispatch := func() (bool, error) {
		if data.Len() == 0 {
			return false, nil
		}
		b := bytes.TrimSpace(data.Bytes())
		if bytes.Equal(b, []byte("[DONE]")) {
			return false, protocolError("missing_response_completed")
		}
		var event wireEvent
		if json.Unmarshal(b, &event) != nil || event.Type == "" {
			return false, protocolError("invalid_sse_event")
		}
		data.Reset()
		return consume(event)
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			done, err := dispatch()
			if done || err != nil {
				return err
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			part := bytes.TrimPrefix(line, []byte("data:"))
			part = bytes.TrimPrefix(part, []byte(" "))
			if data.Len()+len(part)+1 > maxProtocolBytes {
				return protocolError("sse_event_too_large")
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(part)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	done, err := dispatch()
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	return protocolError("missing_response_completed")
}
