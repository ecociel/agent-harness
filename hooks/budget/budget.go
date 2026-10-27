// Package budget caps what runs may consume: tokens (total, input, output),
// money, wall-clock time, model calls and tool calls. It also detects the
// most common runaway, the same tool call repeated with the same arguments.
//
// A Budget is shared state: attach the same *Budget to several agents (or to
// every run of a Session) and they draw from one pool. Child carves a
// sub-budget out of the parent's remaining allowance for a sub-agent; spend
// in the child is debited from the parent too, so a tree of agents can never
// overspend its root. This follows ecociel/agents budget/budget.go.
//
// Checks happen at three points:
//
//   - BeforeModel: refuse the call if a limit is already reached, or if the
//     estimated input alone would exceed the token limit (pre-flight);
//   - AfterModel: charge the real usage reported by the provider;
//   - AfterTurn: count tool calls and detect repeated identical calls.
package budget

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
)

// Limits are the caps. A zero field means "unlimited".
type Limits struct {
	Tokens       int
	InputTokens  int
	OutputTokens int
	ModelCalls   int
	ToolCalls    int
	CostUSD      float64
	Wall         time.Duration
	// RepeatCalls stops a run when one tool is called this many times with
	// identical arguments (0 disables loop detection).
	RepeatCalls int
}

// Price converts tokens to dollars (per million tokens).
type Price struct {
	InputPerM  float64
	OutputPerM float64
}

// Spent is what has been consumed so far.
type Spent struct {
	Usage      model.Usage   `json:"usage"`
	CostUSD    float64       `json:"cost_usd"`
	ModelCalls int           `json:"model_calls"`
	ToolCalls  int           `json:"tool_calls"`
	Elapsed    time.Duration `json:"elapsed_ns"`
}

// Budget is the budget hook and the shared allowance. Safe for concurrent
// use.
type Budget struct {
	limits Limits
	price  Price
	parent *Budget

	mu    sync.Mutex
	start time.Time
	spent Spent
}

// New returns a budget with the given limits.
func New(l Limits) *Budget { return &Budget{limits: l} }

// WithPrice sets the price used for CostUSD and returns b.
func (b *Budget) WithPrice(p Price) *Budget {
	b.mu.Lock()
	b.price = p
	b.mu.Unlock()
	return b
}

// Limits returns the configured caps.
func (b *Budget) Limits() Limits { return b.limits }

// Name implements harness.Hook.
func (b *Budget) Name() string { return "budget" }

// Child returns a sub-budget for a sub-agent: each limit is the smaller of
// want and what b has left (a zero want inherits b's remaining). Spend in the
// child is also charged to b.
func (b *Budget) Child(want Limits) *Budget {
	rem := b.Remaining()
	pick := func(w, r, lim int) int {
		if lim == 0 {
			return w
		}
		if w == 0 || w > r {
			return r
		}
		return w
	}
	l := Limits{
		Tokens:       pick(want.Tokens, rem.Tokens, b.limits.Tokens),
		InputTokens:  pick(want.InputTokens, rem.InputTokens, b.limits.InputTokens),
		OutputTokens: pick(want.OutputTokens, rem.OutputTokens, b.limits.OutputTokens),
		ModelCalls:   pick(want.ModelCalls, rem.ModelCalls, b.limits.ModelCalls),
		ToolCalls:    pick(want.ToolCalls, rem.ToolCalls, b.limits.ToolCalls),
		CostUSD:      want.CostUSD,
		Wall:         want.Wall,
		RepeatCalls:  want.RepeatCalls,
	}
	if b.limits.CostUSD > 0 && (l.CostUSD == 0 || l.CostUSD > rem.CostUSD) {
		l.CostUSD = rem.CostUSD
	}
	if b.limits.Wall > 0 && (l.Wall == 0 || l.Wall > rem.Wall) {
		l.Wall = rem.Wall
	}
	b.mu.Lock()
	price := b.price
	b.mu.Unlock()
	return &Budget{limits: l, price: price, parent: b}
}

// Charge records usage from one model call (and its parents').
func (b *Budget) Charge(u model.Usage) {
	b.mu.Lock()
	b.spent.Usage = b.spent.Usage.Add(u)
	b.spent.ModelCalls++
	b.spent.CostUSD += float64(u.Input)*b.price.InputPerM/1e6 + float64(u.Output)*b.price.OutputPerM/1e6
	b.mu.Unlock()
	if b.parent != nil {
		b.parent.Charge(u)
	}
}

func (b *Budget) chargeTools(n int) {
	b.mu.Lock()
	b.spent.ToolCalls += n
	b.mu.Unlock()
	if b.parent != nil {
		b.parent.chargeTools(n)
	}
}

// Spent returns what has been consumed.
func (b *Budget) Spent() Spent {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.spent
	if !b.start.IsZero() {
		s.Elapsed = time.Since(b.start)
	}
	return s
}

// Remaining returns the allowance left per limit (zero for unlimited fields
// and for exhausted ones; check Limits to tell them apart).
func (b *Budget) Remaining() Limits {
	s := b.Spent()
	sub := func(lim, used int) int { return max(0, lim-used) }
	r := Limits{
		Tokens:       sub(b.limits.Tokens, s.Usage.Total()),
		InputTokens:  sub(b.limits.InputTokens, s.Usage.Input),
		OutputTokens: sub(b.limits.OutputTokens, s.Usage.Output),
		ModelCalls:   sub(b.limits.ModelCalls, s.ModelCalls),
		ToolCalls:    sub(b.limits.ToolCalls, s.ToolCalls),
		CostUSD:      max(0, b.limits.CostUSD-s.CostUSD),
		Wall:         max(0, b.limits.Wall-s.Elapsed),
		RepeatCalls:  b.limits.RepeatCalls,
	}
	return r
}

// Exceeded reports the first limit that is reached, if any.
func (b *Budget) Exceeded() (string, bool) {
	l, s := b.limits, b.Spent()
	switch {
	case l.Tokens > 0 && s.Usage.Total() >= l.Tokens:
		return fmt.Sprintf("token limit %d reached (%d used)", l.Tokens, s.Usage.Total()), true
	case l.InputTokens > 0 && s.Usage.Input >= l.InputTokens:
		return fmt.Sprintf("input token limit %d reached", l.InputTokens), true
	case l.OutputTokens > 0 && s.Usage.Output >= l.OutputTokens:
		return fmt.Sprintf("output token limit %d reached", l.OutputTokens), true
	case l.ModelCalls > 0 && s.ModelCalls >= l.ModelCalls:
		return fmt.Sprintf("model call limit %d reached", l.ModelCalls), true
	case l.ToolCalls > 0 && s.ToolCalls >= l.ToolCalls:
		return fmt.Sprintf("tool call limit %d reached", l.ToolCalls), true
	case l.CostUSD > 0 && s.CostUSD >= l.CostUSD:
		return fmt.Sprintf("cost limit $%.4f reached", l.CostUSD), true
	case l.Wall > 0 && s.Elapsed >= l.Wall:
		return fmt.Sprintf("time limit %s reached", l.Wall), true
	}
	if b.parent != nil {
		if why, ok := b.parent.Exceeded(); ok {
			return "parent budget: " + why, true
		}
	}
	return "", false
}

// BeforeRun starts the wall clock on first use.
func (b *Budget) BeforeRun(context.Context, *harness.Run) error {
	b.mu.Lock()
	if b.start.IsZero() {
		b.start = time.Now()
	}
	b.mu.Unlock()
	if b.parent != nil {
		_ = b.parent.BeforeRun(context.Background(), nil)
	}
	if why, ok := b.Exceeded(); ok {
		return harness.Stop(harness.StopBudget, why)
	}
	return nil
}

// BeforeModel refuses the call when a limit is reached or the estimated
// input would not fit.
func (b *Budget) BeforeModel(_ context.Context, r *harness.Run, req *model.Request) error {
	if why, ok := b.Exceeded(); ok {
		return harness.Stop(harness.StopBudget, why)
	}
	if b.limits.Tokens > 0 {
		est := model.EstimateTokens(req.Messages)
		if left := b.Remaining().Tokens; est > left {
			return harness.Stop(harness.StopBudget,
				fmt.Sprintf("estimated input %d tokens exceeds remaining %d", est, left))
		}
		r.Note(b.Name(), "pre-flight ~%d input tokens, %d left", est, b.Remaining().Tokens)
	}
	return nil
}

// AfterModel charges the real usage.
func (b *Budget) AfterModel(_ context.Context, r *harness.Run, resp *model.Response) error {
	b.Charge(resp.Usage)
	sp := b.Spent()
	if b.limits.Tokens > 0 {
		r.Note(b.Name(), "+%d tokens → %d/%d", resp.Usage.Total(), sp.Usage.Total(), b.limits.Tokens)
	} else {
		r.Note(b.Name(), "+%d tokens → %d", resp.Usage.Total(), sp.Usage.Total())
	}
	return nil
}

// AfterTurn counts tool calls and detects repeated identical calls.
func (b *Budget) AfterTurn(_ context.Context, r *harness.Run) error {
	b.chargeTools(len(r.Last.ToolCalls))
	if b.limits.RepeatCalls > 0 {
		seen, _ := r.Value("budget.repeats").(map[string]int)
		if seen == nil {
			seen = map[string]int{}
		}
		for _, c := range r.Last.ToolCalls {
			k := c.Name + "\x00" + string(c.Arguments)
			seen[k]++
			if seen[k] >= b.limits.RepeatCalls {
				r.Set("budget.repeats", seen)
				return harness.Stop(harness.StopLoop,
					fmt.Sprintf("%s called %d times with identical arguments", c.Name, seen[k]))
			}
		}
		r.Set("budget.repeats", seen)
	}
	if why, ok := b.Exceeded(); ok {
		return harness.Stop(harness.StopBudget, why)
	}
	return nil
}
