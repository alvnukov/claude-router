package anthropicerror

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Category uint8

const (
	UpstreamFailure Category = iota
	Transient429
	Quota429
	Unknown429
)

// Failure holds only public-safe classification, never an upstream message or header.
type Failure struct {
	Status     int
	Category   Category
	RetryAfter time.Duration
	HTTPStatus int
}

type envelope struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func safeEnvelope(f Failure) envelope {
	v := envelope{Type: "error"}
	v.Error.Type = "api_error"
	v.Error.Message = "The upstream service failed."
	if f.Status != http.StatusTooManyRequests || f.HTTPStatus != http.StatusTooManyRequests {
		return v
	}
	switch f.Category {
	case Transient429:
		v.Error.Type, v.Error.Message = "rate_limit_error", "The upstream service reported a rate limit."
	case Quota429:
		v.Error.Type, v.Error.Message = "rate_limit_error", "The upstream service reported a usage or spending limit."
	case Unknown429:
		v.Error.Type, v.Error.Message = "rate_limit_error", "The upstream service reported a limit."
	}
	return v
}

func WriteHTTP(w http.ResponseWriter, f Failure) {
	h := w.Header()
	for k := range h {
		if strings.EqualFold(k, "Retry-After") || strings.EqualFold(k, "Content-Length") {
			delete(h, k)
		}
	}
	if f.Status == http.StatusTooManyRequests && f.HTTPStatus == http.StatusTooManyRequests && f.Category == Transient429 && f.RetryAfter > 0 && f.RetryAfter <= 24*time.Hour {
		seconds := (f.RetryAfter + time.Second - 1) / time.Second
		h.Set("Retry-After", strconv.FormatInt(int64(seconds), 10))
	}
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	status := f.Status
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(safeEnvelope(f))
}

// WriteSSE writes one complete error frame; the caller owns commit and terminal state.
func WriteSSE(w http.ResponseWriter, f Failure) error {
	body, err := json.Marshal(safeEnvelope(f))
	if err != nil {
		return err
	}
	frame := append(append([]byte("event: error\ndata: "), body...), '\n', '\n')
	n, err := w.Write(frame)
	if err != nil {
		return err
	}
	if n != len(frame) {
		return io.ErrShortWrite
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}
