package agent

import "time"

// TokenStats tracks cumulative token usage for an agent session.
type TokenStats struct {
	TotalInputTokens         int64
	TotalCacheReadTokens     int64 // 0.1x billing rate
	TotalCacheCreationTokens int64 // 1.25x billing rate at 5m TTL
	TotalOutputTokens        int64
	TotalCostUSD             float64
	APICallCount             int
	Model                    string
}

// TotalInputTokensInclCache returns the full API-side input volume:
// uncached + cache_read + cache_creation.
func (s TokenStats) TotalInputTokensInclCache() int64 {
	return s.TotalInputTokens + s.TotalCacheReadTokens + s.TotalCacheCreationTokens
}

// ModelCostBreakdown reports cost attribution per model.
type ModelCostBreakdown struct {
	Calls         int     `json:"calls"`
	ActualCost    float64 `json:"actual_cost"`
	WouldHaveCost float64 `json:"would_have_cost"`
	Saved         float64 `json:"saved"`
	InputTokens   int     `json:"input_tokens"`
	OutputTokens  int     `json:"output_tokens"`
}

// CostSavingsReport summarises routing cost savings over a time window.
type CostSavingsReport struct {
	PeriodHours       int                           `json:"period_hours"`
	StartTime         time.Time                     `json:"start_time"`
	EndTime           time.Time                     `json:"end_time"`
	ActualCost        float64                       `json:"actual_cost"`
	DefaultModelCost  float64                       `json:"default_model_cost"`
	SavingsCost       float64                       `json:"savings_cost"`
	SavingsPercent    float64                       `json:"savings_percent"`
	TotalRequests     int                           `json:"total_requests"`
	ByModel           map[string]ModelCostBreakdown `json:"by_model"`
	RoutingMethodStat map[string]int                `json:"routing_method_stat"`
}
