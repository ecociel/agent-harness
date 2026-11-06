package model

import "strings"

// Price is what one model costs per million tokens, in micro-units of the
// consumer's currency. Cached input is priced on its own. Reasoning, when zero,
// is priced as Output.
type Price struct {
	InputPerM     int64 `json:"input_per_m"`
	CachedPerM    int64 `json:"cached_per_m"`
	OutputPerM    int64 `json:"output_per_m"`
	ReasoningPerM int64 `json:"reasoning_per_m,omitempty"`
}

// Prices maps a model name to its Price. A key ending in "*" matches every name
// with that prefix, and "*" alone is the default for any model. An exact name
// wins over a prefix, and the longest prefix wins over a shorter one.
type Prices map[string]Price

// Lookup returns the Price for the named model.
func (p Prices) Lookup(name string) (Price, bool) {
	if pr, ok := p[name]; ok {
		return pr, true
	}
	var (
		best  Price
		bestN = -1
	)
	for k, pr := range p {
		prefix, ok := strings.CutSuffix(k, "*")
		if ok && strings.HasPrefix(name, prefix) && len(prefix) > bestN {
			best, bestN = pr, len(prefix)
		}
	}
	return best, bestN >= 0
}

// Cost returns the cost of u on the named model in micro-units, and false for a
// model with no price, so a caller can refuse to run unpriced models instead of
// billing zero. Cached tokens are a subset of Input and reasoning tokens a subset
// of Output, so each is priced once, at its own rate. The sum is rounded to the
// nearest micro-unit.
func (p Prices) Cost(name string, u Usage) (micros int64, ok bool) {
	pr, ok := p.Lookup(name)
	if !ok {
		return 0, false
	}
	in, out := max(u.Input, 0), max(u.Output, 0)
	cached := min(max(u.Cached, 0), in)
	reasoning := min(max(u.Reasoning, 0), out)
	rate := pr.ReasoningPerM
	if rate == 0 {
		rate = pr.OutputPerM
	}
	sum := int64(in-cached)*pr.InputPerM + int64(cached)*pr.CachedPerM +
		int64(out-reasoning)*pr.OutputPerM + int64(reasoning)*rate
	return (sum + 500_000) / 1_000_000, true
}
