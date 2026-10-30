// Package budget caps what a run may consume: tokens (total, input, output),
// money, wall-clock time, model calls and tool calls. It also detects the
// most common runaway, the same tool call repeated with the same arguments.
//
// A Budget holds limits and no spend. Everything it compares against comes from the
// Run it is handed, which is the fold of the journal, so a budget survives a crash,
// a resume on another machine and a fresh Budget value with nothing to restore.
//
// A Run knows its own spend, not its tree's or its tenant's. Those totals are the
// consumer's: project usage in Journal.Append, where (RunID, Seq) is unique, so each
// reply counts once, and hand the projection to WithPool. Parallel branches race the
// check, so a shared limit can overshoot by the calls already in flight.
//
// Checks happen at two points:
//
//   - BeforeRun: refuse a worker if a limit is already reached;
//   - BeforeModel: refuse the call if a limit is reached, if the same tool call has
//     repeated too often, or if the estimated input alone would exceed the token
//     limit (pre-flight).
package budget

import (
	"context"
	"fmt"
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
	Usage model.Usage `json:"usage"`
	// ModelCalls counts every call that may have been billed: answered ones and
	// ones a crash cut off.
	ModelCalls int           `json:"model_calls"`
	ToolCalls  int           `json:"tool_calls"`
	CostUSD    float64       `json:"cost_usd"`
	Elapsed    time.Duration `json:"elapsed_ns"`
}

// Pool reports what a whole tree or tenant has spent, from the consumer's own
// projection of the journal.
type Pool func(ctx context.Context, r harness.Run) (Spent, error)

// Budget is the budget hook. Safe for concurrent use: it holds nothing that changes.
type Budget struct {
	limits Limits
	price  Price
	pool   Pool
}

// New returns a budget with the given limits.
func New(l Limits) *Budget { return &Budget{limits: l} }

// WithPrice sets the price used for CostUSD and returns b.
func (b *Budget) WithPrice(p Price) *Budget {
	b.price = p
	return b
}

// WithPool makes the limits apply to what pool reports instead of to one run.
func (b *Budget) WithPool(pool Pool) *Budget {
	b.pool = pool
	return b
}

// Limits returns the configured caps.
func (b *Budget) Limits() Limits { return b.limits }

// Name implements harness.Hook.
func (b *Budget) Name() string { return "budget" }

// Spent derives what r has consumed. Time spent suspended is not counted: a run
// waiting a week for a signal has used none of its wall-clock limit.
func (b *Budget) Spent(r harness.Run) Spent {
	return Spent{Usage: r.Usage, ModelCalls: r.ModelCalls + r.LostCalls, ToolCalls: r.ToolCalls,
		CostUSD: float64(r.Usage.Input)*b.price.InputPerM/1e6 + float64(r.Usage.Output)*b.price.OutputPerM/1e6,
		Elapsed: time.Since(r.Started) - r.Waited}
}

func (b *Budget) current(ctx context.Context, r harness.Run) (Spent, error) {
	if b.pool == nil {
		return b.Spent(r), nil
	}
	return b.pool(ctx, r)
}

// Exceeded reports the first limit s has reached, if any.
func (b *Budget) Exceeded(s Spent) (string, bool) {
	l := b.limits
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
	return "", false
}

// repeats returns a call the run has now made RepeatCalls times with identical
// arguments, read from the recorded replies.
func (b *Budget) repeats(r harness.Run) (string, bool) {
	seen := map[string]int{}
	for _, e := range r.Events {
		if e.Kind != harness.KindModelDone {
			continue
		}
		for _, c := range e.Reply.ToolCalls {
			k := c.Name + "\x00" + string(c.Arguments)
			seen[k]++
			if seen[k] >= b.limits.RepeatCalls {
				return fmt.Sprintf("%s called %d times with identical arguments", c.Name, seen[k]), true
			}
		}
	}
	return "", false
}

func (b *Budget) check(ctx context.Context, r harness.Run) (Spent, error) {
	s, err := b.current(ctx, r)
	if err != nil {
		return s, err
	}
	if why, ok := b.Exceeded(s); ok {
		return s, harness.Stop(harness.StopBudget, why)
	}
	return s, nil
}

// BeforeRun refuses a worker when a limit is already reached.
func (b *Budget) BeforeRun(ctx context.Context, r harness.Run) error {
	_, err := b.check(ctx, r)
	return err
}

// BeforeModel refuses the call when a limit is reached, a call repeats, or the
// estimated input would not fit.
func (b *Budget) BeforeModel(ctx context.Context, r harness.Run, req *model.Request) error {
	s, err := b.check(ctx, r)
	if err != nil {
		return err
	}
	if b.limits.RepeatCalls > 0 {
		if why, ok := b.repeats(r); ok {
			return harness.Stop(harness.StopLoop, why)
		}
	}
	if b.limits.Tokens > 0 {
		est := model.EstimateTokens(req.Messages)
		left := max(0, b.limits.Tokens-s.Usage.Total())
		if est > left {
			return harness.Stop(harness.StopBudget, fmt.Sprintf("estimated input %d tokens exceeds remaining %d", est, left))
		}
		harness.Note(ctx, "pre-flight ~%d input tokens, %d left", est, left)
	}
	return nil
}
