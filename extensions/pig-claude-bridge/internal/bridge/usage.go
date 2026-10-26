package bridge

import "math"

// SdkUsage is the usage counters Claude Code reports (usage.ts SdkUsage). Every
// field is a pointer because message_start, message_delta and a complete
// assistant message each carry a different subset; nil means absent.
type SdkUsage struct {
	InputTokens              *int
	OutputTokens             *int
	CacheReadInputTokens     *int
	CacheCreationInputTokens *int
	// ThinkingTokens is output_tokens_details.thinking_tokens; no flat spelling exists.
	ThinkingTokens *int
}

// UsageReport is what updateUsage derived but does not store on the message.
type UsageReport struct {
	// CachePct is the cache-read share of the prompt as a whole percentage; 0 when the prompt is empty.
	CachePct int
	// Reasoning is the thinking tokens carried by THIS frame, nil when it carried none.
	// Distinct from output.usage.reasoning, which keeps the last value seen.
	Reasoning *int
}

// parseSdkUsage reads a decoded usage object; anything that is not a number is absent.
func parseSdkUsage(raw map[string]any) SdkUsage {
	details, _ := raw["output_tokens_details"].(map[string]any)
	return SdkUsage{
		InputTokens:              optInt(raw["input_tokens"]),
		OutputTokens:             optInt(raw["output_tokens"]),
		CacheReadInputTokens:     optInt(raw["cache_read_input_tokens"]),
		CacheCreationInputTokens: optInt(raw["cache_creation_input_tokens"]),
		ThinkingTokens:           optInt(details["thinking_tokens"]),
	}
}

func optInt(v any) *int {
	if f, ok := number(v); ok {
		n := int(f)
		return &n
	}
	return nil
}

// number reads any numeric representation a decoded frame or a Go-built map may hold.
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func intOf(v any) int {
	f, _ := number(v)
	return int(f)
}

// updateUsage applies one SDK usage frame onto output["usage"], then recomputes
// the token total and cost. Counters the frame omits keep their previous value.
func updateUsage(output map[string]any, u SdkUsage, model map[string]any) UsageReport {
	usage, _ := output["usage"].(map[string]any)
	if usage == nil {
		usage = map[string]any{}
		output["usage"] = usage
	}
	if u.InputTokens != nil {
		usage["input"] = *u.InputTokens
	}
	if u.OutputTokens != nil {
		usage["output"] = *u.OutputTokens
	}
	if u.CacheReadInputTokens != nil {
		usage["cacheRead"] = *u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens != nil {
		usage["cacheWrite"] = *u.CacheCreationInputTokens
	}
	// A subset of output tokens, never a separate addend in the total or in cost.
	if u.ThinkingTokens != nil {
		usage["reasoning"] = *u.ThinkingTokens
	}
	input, out, cacheRead, cacheWrite := intOf(usage["input"]), intOf(usage["output"]), intOf(usage["cacheRead"]), intOf(usage["cacheWrite"])
	usage["totalTokens"] = input + out + cacheRead + cacheWrite
	calculateCost(model, usage, input, out, cacheRead, cacheWrite)
	report := UsageReport{Reasoning: u.ThinkingTokens}
	if prompt := input + cacheRead + cacheWrite; prompt > 0 {
		report.CachePct = int(math.Round(float64(cacheRead) / float64(prompt) * 100))
	}
	return report
}

// calculateCost is pi-ai's calculateCost: per-million-token rates from the model's
// cost fields. The bridge zeroes those rates, so real cost is zero. pi-ai's
// cost.tiers and usage.cacheWrite1h are left out: with zero rates, and no 1h cache
// writes reported by Claude Code, they give the same zeros.
func calculateCost(model, usage map[string]any, input, out, cacheRead, cacheWrite int) {
	rates, _ := model["cost"].(map[string]any)
	per := func(rate any, tokens int) float64 {
		r, _ := number(rate)
		return r / 1_000_000 * float64(tokens)
	}
	cost := map[string]any{
		"input":      per(rates["input"], input),
		"output":     per(rates["output"], out),
		"cacheRead":  per(rates["cacheRead"], cacheRead),
		"cacheWrite": per(rates["cacheWrite"], cacheWrite),
	}
	cost["total"] = cost["input"].(float64) + cost["output"].(float64) + cost["cacheRead"].(float64) + cost["cacheWrite"].(float64)
	usage["cost"] = cost
}
