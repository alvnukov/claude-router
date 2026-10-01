package codex

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"localrouter/internal/providers"
)

const maxProtocolBytes = 16 << 20

// ProtocolError distinguishes terminal request errors from transient upstream
// failures. Its public text never includes upstream bodies or credentials.
type ProtocolError struct {
	Code           string
	Status         int
	Retryable      bool
	RetryAfter     time.Duration
	HTTPStatus     int
	validHTTPError bool
	retryHeader    string
	observedAt     time.Time
}

func (e *ProtocolError) Error() string { return "Codex: " + e.Code }

func protocolError(code string) error {
	return &ProtocolError{Code: code, Status: http.StatusBadGateway}
}

func upstreamError(code string, status int) *ProtocolError {
	e := &ProtocolError{Code: "upstream_error", Status: status, Retryable: status == 408 || status == 429 || status >= 500}
	switch code {
	case "subscription_sharing_usage_limit_exceeded":
		e.Code, e.Status, e.Retryable = code, 429, true
	case "subscription_sharing_usage_unavailable", "subscription_sharing_user_unavailable":
		e.Code, e.Status, e.Retryable = code, 503, true
	case "subscription_sharing_user_not_eligible", "subscription_sharing_route_not_supported", "chatpass_v2_scope_not_authorized", "chatpass_v2_invalid_authorization_context":
		e.Code, e.Status, e.Retryable = code, 403, false
	case "subscription_sharing_unsupported_capability":
		e.Code, e.Status, e.Retryable = code, 400, false
	case "subscription_sharing_invalid_user":
		e.Code, e.Status, e.Retryable = code, 401, false
	case "context_length_exceeded", "invalid_prompt", "invalid_request_error":
		e.Code, e.Status, e.Retryable = code, 400, false
	case "insufficient_quota", "credit_balance_exhausted", "organization_spend_limit_exceeded", "project_spend_limit_exceeded", "usage_not_included", "usage_limit_reached":
		e.Code, e.Status, e.Retryable = code, 429, true
	case "cyber_policy", "bio_policy", "misalignment_policy_violation", "content_policy_violation":
		e.Code, e.Status, e.Retryable = code, 403, false
	case "rate_limit_exceeded", "slow_down":
		e.Code, e.Status, e.Retryable = code, 429, true
	case "server_is_overloaded", "server_error", "flex_capacity_exceeded", "flex_unavailable":
		e.Code, e.Status, e.Retryable = code, 503, true
	}
	return e
}

type wireError struct {
	Code string `json:"code"`
	Type string `json:"type"`
}

type wireItem struct {
	Type      string     `json:"type"`
	ID        string     `json:"id"`
	Role      string     `json:"role"`
	CallID    string     `json:"call_id"`
	Name      string     `json:"name"`
	Namespace string     `json:"namespace"`
	Arguments string     `json:"arguments"`
	Content   []TextPart `json:"content"`
}

type wireEvent struct {
	Headers      map[string]json.RawMessage `json:"headers"`
	Type         string                     `json:"type"`
	Delta        string                     `json:"delta"`
	Text         string                     `json:"text"`
	ItemID       string                     `json:"item_id"`
	OutputIndex  int                        `json:"output_index"`
	ContentIndex int                        `json:"content_index"`
	SummaryIndex int                        `json:"summary_index"`
	Item         json.RawMessage            `json:"item"`
	Error        wireError                  `json:"error"`
	Response     struct {
		ID         string            `json:"id"`
		Status     string            `json:"status"`
		EndTurn    *bool             `json:"end_turn"`
		Output     []json.RawMessage `json:"output"`
		Usage      json.RawMessage   `json:"usage"`
		Error      wireError         `json:"error"`
		Incomplete struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	} `json:"response"`
}

// ToolCall is the Chat-compatible projection of a completed native call. The
// original item, including IDs and encrypted metadata, remains in Output.
type ToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Index    *int   `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type Delta struct {
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// Completion retains the native output independently of its client projection.
// Usage totals all sampling calls; LastUsage describes the current context.
type Completion struct {
	Text string
	// VisibleText mirrors the client's joined text blocks when streaming split
	// text around thinking or tool blocks. Nil uses the blocking projection.
	VisibleText *string
	Tools       []ToolCall
	Output      []json.RawMessage
	ResponseIDs []string
	Usage       providers.ResponsesUsage
	LastUsage   providers.ResponsesUsage
	UsageKnown  bool
	EndTurn     *bool
	TurnState   string
	Calls       int
	Started     bool // meaningful output has begun, so replay may duplicate charged work
}

func (c Completion) ChatResponse() []byte {
	finish := "stop"
	if len(c.Tools) != 0 {
		finish = "tool_calls"
	}
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": c.Text, "tool_calls": c.Tools}, "finish_reason": finish}},
		"usage":   c.LastUsage.Chat(),
	})
	return b
}

func (c Completion) VisibleOutput() []json.RawMessage {
	var items []json.RawMessage
	text := c.Text
	if c.VisibleText != nil {
		text = *c.VisibleText
	}
	if text != "" {
		b, _ := json.Marshal(map[string]any{"type": "message", "role": "assistant", "content": text})
		items = append(items, b)
	}
	for _, call := range c.Tools {
		b, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments})
		items = append(items, b)
	}
	return items
}

func validateArguments(args string) error {
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &object) != nil || object == nil {
		return protocolError("invalid_function_arguments")
	}
	return nil
}

func rawItemsSize(items []json.RawMessage) int {
	n := 0
	for _, item := range items {
		n += len(item)
	}
	return n
}

func validHeader(value string) bool {
	if len(value) > 8192 {
		return false
	}
	for _, c := range value {
		if c < 32 || c > 126 {
			return false
		}
	}
	return true
}

func decodeRequest(payload []byte) (map[string]json.RawMessage, []json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
		return nil, nil, protocolError("invalid_request")
	}
	var input []json.RawMessage
	if err := json.Unmarshal(fields["input"], &input); err != nil {
		return nil, nil, protocolError("invalid_input")
	}
	return fields, input, nil
}

func encodeRequest(fields map[string]json.RawMessage, input []json.RawMessage) ([]byte, error) {
	b, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode Codex input: %w", err)
	}
	fields["input"] = b
	return json.Marshal(fields)
}
