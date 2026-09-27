package providers

import "encoding/json"

// InputTokenDetails preserves unknown versus explicitly zero cache reads.
type InputTokenDetails struct {
	CachedTokens     *int `json:"cached_tokens,omitempty"`
	CacheWriteTokens *int `json:"cache_write_tokens,omitempty"`
}

type OutputTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ResponsesUsage is the usage reported by Codex's Responses endpoint.
type ResponsesUsage struct {
	InputTokens        int                 `json:"input_tokens"`
	OutputTokens       int                 `json:"output_tokens"`
	InputTokenDetails  *InputTokenDetails  `json:"input_tokens_details,omitempty"`
	OutputTokenDetails *OutputTokenDetails `json:"output_tokens_details,omitempty"`
	TotalTokens        int                 `json:"total_tokens,omitempty"`
	BudgetUnits        json.Number         `json:"codex_rollout_budget_units,omitempty"`
}

// ChatUsage is shared by streaming and blocking OpenAI-compatible responses.
type ChatUsage struct {
	PromptTokens           int                 `json:"prompt_tokens"`
	CompletionTokens       int                 `json:"completion_tokens"`
	PromptTokenDetails     *InputTokenDetails  `json:"prompt_tokens_details,omitempty"`
	CompletionTokenDetails *OutputTokenDetails `json:"completion_tokens_details,omitempty"`
}

func (u ResponsesUsage) Chat() ChatUsage {
	return ChatUsage{
		PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens,
		PromptTokenDetails:     u.InputTokenDetails,
		CompletionTokenDetails: u.OutputTokenDetails,
	}
}

// Anthropic separates cache reads from new input, while OpenAI includes them
// in the total input. Never invent a cache hit or subtract an invalid count.
func (u ChatUsage) Anthropic() map[string]int {
	usage := map[string]int{
		"input_tokens": u.PromptTokens, "output_tokens": u.CompletionTokens,
	}
	if d := u.PromptTokenDetails; d != nil {
		cached, written := 0, 0
		if d.CachedTokens != nil {
			cached = *d.CachedTokens
		}
		if d.CacheWriteTokens != nil {
			written = *d.CacheWriteTokens
		}
		if cached >= 0 && written >= 0 && cached <= u.PromptTokens && written <= u.PromptTokens-cached {
			usage["input_tokens"] -= cached + written
			if d.CachedTokens != nil {
				usage["cache_read_input_tokens"] = cached
			}
			if d.CacheWriteTokens != nil {
				usage["cache_creation_input_tokens"] = written
			}
		}
	}
	return usage
}

// Add aggregates billed sampling work. Missing detail remains unknown rather
// than becoming an observed zero for an entire multi-call response.
func (u *ResponsesUsage) Add(v ResponsesUsage) {
	u.InputTokens += v.InputTokens
	u.OutputTokens += v.OutputTokens
	u.TotalTokens += v.TotalTokens
	if u.InputTokenDetails != nil && v.InputTokenDetails != nil {
		u.InputTokenDetails = &InputTokenDetails{CachedTokens: addKnown(u.InputTokenDetails.CachedTokens, v.InputTokenDetails.CachedTokens), CacheWriteTokens: addKnown(u.InputTokenDetails.CacheWriteTokens, v.InputTokenDetails.CacheWriteTokens)}
	} else {
		u.InputTokenDetails = nil
	}
	if u.OutputTokenDetails != nil && v.OutputTokenDetails != nil {
		u.OutputTokenDetails = &OutputTokenDetails{ReasoningTokens: u.OutputTokenDetails.ReasoningTokens + v.OutputTokenDetails.ReasoningTokens}
	} else {
		u.OutputTokenDetails = nil
	}
	// Budget-unit metadata is per call, not a stable token conversion rate.
	u.BudgetUnits = ""
}

func addKnown(a, b *int) *int {
	if a == nil || b == nil {
		return nil
	}
	value := *a + *b
	return &value
}
