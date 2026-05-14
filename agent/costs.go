package agent

import "github.com/chimerakang/alice-core/client"

// ModelPricing holds per-model token rates (USD per million tokens).
var ModelPricing = map[string]struct {
	InputPerMTok  float64
	OutputPerMTok float64
}{
	"haiku":  {1.00, 5.00},
	"sonnet": {3.00, 15.00},
	"opus":   {15.00, 75.00},
}

// EstimateClaudeCost returns a USD cost estimate for one CLI call using
// Anthropic prompt-cache pricing. Returns 0 when the model is unrecognised.
func EstimateClaudeCost(model string, inputTokens, cacheReadTokens, cacheCreationTokens, outputTokens int) float64 {
	short := client.ExtractModelShortName(model)
	rate, ok := ModelPricing[short]
	if !ok {
		return 0
	}
	weighted := float64(inputTokens) +
		float64(cacheReadTokens)*0.1 +
		float64(cacheCreationTokens)*1.25
	return (weighted*rate.InputPerMTok + float64(outputTokens)*rate.OutputPerMTok) / 1_000_000
}
