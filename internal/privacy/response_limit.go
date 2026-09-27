package privacy

import (
	"errors"
	"io"
)

type responseLimitReader struct {
	source    io.Reader
	remaining int64
}

// NewResponseReader rejects an extra byte instead of treating a full limit as EOF.
func NewResponseReader(source io.Reader) io.Reader {
	return &responseLimitReader{source: source, remaining: TrafficOutputLimit}
}

func (r *responseLimitReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		var probe [1]byte
		n, err := r.source.Read(probe[:])
		if n > 0 {
			return 0, errors.New("privacy upstream response limit")
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.source.Read(p)
	r.remaining -= int64(n)
	return n, err
}
