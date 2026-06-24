package core

// Token-usage accounting and budget enforcement.
//
// Phase-1 sibling of the reference engines' cost tracking. Accumulates token
// usage across a turn's model calls, optionally converts it to a USD cost via a
// per-model pricing table, and lets a turn stop early once a token or cost budget
// is hit. Usage is exact; cost depends on the (approximate, overridable) pricing.

// Usage holds exact token counts reported by the model API.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

// TotalTokens is prompt + completion.
func (u Usage) TotalTokens() int { return u.PromptTokens + u.CompletionTokens }

// ModelPricing is USD per 1,000,000 tokens, input and output.
type ModelPricing struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

// Cost converts usage to USD at this pricing.
func (p ModelPricing) Cost(u Usage) float64 {
	return (float64(u.PromptTokens)*p.InputPerMTok + float64(u.CompletionTokens)*p.OutputPerMTok) / 1_000_000
}

// DefaultPricing is approximate (USD / 1M tokens). Override via AgentOptions.Pricing.
var DefaultPricing = map[string]ModelPricing{
	"claude-haiku-4-5":  {InputPerMTok: 1.0, OutputPerMTok: 5.0},
	"claude-sonnet-4-5": {InputPerMTok: 3.0, OutputPerMTok: 15.0},
}

// CostBudget is a ceiling for a turn. A zero field means "unset"; the first
// non-zero limit that is hit stops the turn.
type CostBudget struct {
	MaxUSD    float64
	MaxTokens int
}

// CostTracker accumulates usage + cost across a turn's model calls.
type CostTracker struct {
	Usage   Usage
	CostUSD float64
}

// Record adds a model call's usage (and its cost, if the model is priced).
func (t *CostTracker) Record(model string, u Usage, pricing map[string]ModelPricing) {
	t.Usage.PromptTokens += u.PromptTokens
	t.Usage.CompletionTokens += u.CompletionTokens
	table := pricing
	if table == nil {
		table = DefaultPricing
	}
	if mp, ok := table[model]; ok {
		t.CostUSD += mp.Cost(u)
	}
}

// Exceeds reports whether the accumulated usage/cost has hit the budget.
func (t *CostTracker) Exceeds(b *CostBudget) bool {
	if b == nil {
		return false
	}
	if b.MaxTokens > 0 && t.Usage.TotalTokens() >= b.MaxTokens {
		return true
	}
	if b.MaxUSD > 0 && t.CostUSD >= b.MaxUSD {
		return true
	}
	return false
}
