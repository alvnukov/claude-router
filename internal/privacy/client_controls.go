package privacy

import (
	"slices"
	"strconv"
)

// ClientControlSupport records an approved direct-Anthropic model/beta pair
// with the model's independent token limits. Production has no entries yet.
type ClientControlSupport struct {
	Model           string
	Betas           []string
	MaxTokens       int64
	MaxBudgetTokens int64
}

type clientControlTable map[string]ClientControlSupport

func newClientControlTable(support []ClientControlSupport) clientControlTable {
	table := make(clientControlTable, len(support))
	for _, entry := range support {
		entry.Betas = append([]string(nil), entry.Betas...)
		table[entry.Model] = entry
	}
	return table
}

func (table clientControlTable) clone() clientControlTable {
	copy := make(clientControlTable, len(table))
	for model, entry := range table {
		entry.Betas = append([]string(nil), entry.Betas...)
		copy[model] = entry
	}
	return copy
}

func (table clientControlTable) allows(target Target, body []byte) bool {
	if target.Translated || target.Provider != "anthropic" {
		return false
	}
	n, err := scanJSON(body)
	if err != nil || n.kind != '{' || n.str("model") != target.Model {
		return false
	}
	entry, ok := table[target.Model]
	if !ok || entry.MaxTokens <= 0 || entry.MaxBudgetTokens <= 0 || !slices.Equal(target.Betas, entry.Betas) || len(entry.Betas) == 0 {
		return false
	}
	thinking, max := n.get("thinking"), n.get("max_tokens")
	if thinking == nil || max == nil {
		return false
	}
	budget := thinking.get("budget_tokens")
	if budget == nil {
		return false
	}
	maxValue, maxErr := strconv.ParseInt(string(body[max.start:max.end]), 10, 64)
	budgetValue, budgetErr := strconv.ParseInt(string(body[budget.start:budget.end]), 10, 64)
	return maxErr == nil && budgetErr == nil && maxValue > 0 && maxValue <= entry.MaxTokens && budgetValue > 0 && budgetValue <= entry.MaxBudgetTokens && budgetValue < maxValue
}
