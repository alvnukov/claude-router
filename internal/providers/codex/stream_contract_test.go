package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStartReturnsMatchingStreamingAndBufferedCompletion(t *testing.T) {
	wire := contractSSE(contractItemEvent(0, `{"type":"message","id":"m","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"answer"}]}`),
		contractCompleted("r", `,"usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110,"input_tokens_details":{"cached_tokens":90}}`))
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streaming"}[streaming], func(t *testing.T) {
			send := func(context.Context, []byte, http.Header) (*http.Response, error) {
				return contractHTTPResponse(wire, "state"), nil
			}
			stream := Start(context.Background(), contractRequest(contractUser), send, Options{}, streaming)
			defer stream.Close()
			body, err := io.ReadAll(stream)
			if err != nil {
				t.Fatal(err)
			}
			result, err := stream.Result()
			if err != nil || result.Text != "answer" || result.TurnState != "state" || len(result.Output) != 1 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if streaming {
				if strings.Count(string(body), "data: [DONE]") != 1 || !strings.Contains(string(body), `"content":"answer"`) || !strings.Contains(string(body), `"cached_tokens":90`) {
					t.Fatalf("bad chat stream: %s", body)
				}
				return
			}
			var chat struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
					Finish string `json:"finish_reason"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(body, &chat); err != nil || len(chat.Choices) != 1 || chat.Choices[0].Message.Content != "answer" || chat.Choices[0].Finish != "stop" {
				t.Fatalf("bad buffered chat response: %s, err=%v", body, err)
			}
		})
	}
}

func TestStartNeverMarksFailedPartialOutputSuccessful(t *testing.T) {
	send := func(context.Context, []byte, http.Header) (*http.Response, error) {
		return contractHTTPResponse(contractSSE(`{"type":"response.output_text.delta","delta":"partial"}`,
			`{"type":"response.failed","response":{"id":"r","error":{"code":"rate_limit_exceeded"}}}`), ""), nil
	}
	stream := Start(context.Background(), contractRequest(contractUser), send, Options{}, true)
	defer stream.Close()
	body, readErr := io.ReadAll(stream)
	_, resultErr := stream.Result()
	if readErr == nil || resultErr == nil || strings.Contains(string(body), "[DONE]") || strings.Contains(string(body), "finish_reason") {
		t.Fatalf("partial failure looked successful: body=%s read=%v result=%v", body, readErr, resultErr)
	}
}

func TestStartCloseCancelsUpstreamAndUnblocksResult(t *testing.T) {
	requestStarted := make(chan struct{})
	requestCancelled := make(chan struct{})
	send := func(ctx context.Context, _ []byte, _ http.Header) (*http.Response, error) {
		close(requestStarted)
		body := contractCancelObservedReader{ctx: ctx, observed: requestCancelled}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(body)}, nil
	}
	stream := Start(context.Background(), contractRequest(contractUser), send, Options{}, true)
	defer stream.Close()
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request did not start")
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream read did not receive cancellation")
	}
	done := make(chan error, 1)
	go func() { _, err := stream.Result(); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled stream completed successfully or lost cause: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Result stayed blocked after Close")
	}
}

type contractCancelObservedReader struct {
	ctx      context.Context
	observed chan struct{}
}

func (r contractCancelObservedReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	close(r.observed)
	return 0, r.ctx.Err()
}
