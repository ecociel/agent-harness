package multi

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/ecociel/agent-harness"
)

// Parallel fans one input out to several branches at once and joins the
// results (fan-out / fan-in).
//
//	      ┌→ branch 1 ─┐
//	in ───┼→ branch 2 ─┼→ join → out
//	      └→ branch 3 ─┘
//
// Each branch has its own context window, so parallel review by specialists
// costs no more context per agent than one review. By default the join is
// code, not a model: every branch must complete, and the outputs are
// concatenated under headers. Set Aggregator to have a model synthesise them
// instead, or Join for full control.
//
// A branch that waits does not hold up the others. Every branch runs as far as it
// can, each to its own journal, and then the pattern suspends on all open waits.
// Once every wait is answered the join runs over what the branches recorded.
type Parallel struct {
	Name     string
	Branches []harness.Runner
	// MaxConcurrent bounds concurrency (0 = all at once).
	MaxConcurrent int
	// Aggregator, if set, receives the original request plus all branch
	// outputs and produces the final answer.
	Aggregator harness.Runner
	// Join, if set, replaces both the default join and Aggregator. It must be a pure
	// function of its arguments: it runs again after a crash.
	Join func(ctx context.Context, in harness.Input, results []*harness.Run) (string, error)
	// AllowPartial accepts results even if some branches did not complete.
	AllowPartial bool
	// Journal makes the pattern durable. Nil inherits, like Agent.Journal.
	Journal harness.Journal
	// Hooks attach to the pattern's own run. A Redactor here covers every branch.
	Hooks []harness.Hook
}

// RunnerName implements harness.Named.
func (p *Parallel) RunnerName() string { return p.Name }

// Run implements harness.Runner.
func (p *Parallel) Run(ctx context.Context, in harness.Input) (*harness.Run, error) {
	parts := names(p.Branches)
	if p.Aggregator != nil {
		parts = append(parts, "aggregate:"+harness.NameOf(p.Aggregator))
	}
	spec := pattern{kind: "parallel", name: p.Name, journal: p.Journal, hooks: p.Hooks, parts: parts}
	return spec.run(ctx, in, func(s *scope) (string, error) { return p.body(s) })
}

func (p *Parallel) body(s *scope) (string, error) {
	in := s.in
	results, errs := fanOut(s, p.Branches, func(int) harness.Input { return in }, p.MaxConcurrent)
	var failed []string
	var waits []harness.Wait
	for i, r := range results {
		switch {
		case r == nil:
			return "", errs[i]
		case r.State == harness.StateSuspended:
			waits = append(waits, r.Waits...)
		case errs[i] != nil:
			failed = append(failed, fmt.Sprintf("%s: %v", harness.NameOf(p.Branches[i]), errs[i]))
		case !r.OK():
			failed = append(failed, fmt.Sprintf("%s: %s", harness.NameOf(p.Branches[i]), r.Stop))
		}
	}
	// A doomed pattern must not ask a human to approve work it will throw away.
	if len(failed) > 0 && !p.AllowPartial {
		return s.fail(errors.New("branches did not complete: " + strings.Join(failed, "; ")))
	}
	if len(waits) > 0 {
		return s.park(waits...)
	}
	s.emit("join", fmt.Sprintf("%d branches", len(results)))
	switch {
	case p.Join != nil:
		out, err := p.Join(s.ctx, in, results)
		if err != nil {
			return s.fail(err)
		}
		return s.ok(out)
	case p.Aggregator != nil:
		var outs []string
		for _, r := range results {
			outs = append(outs, r.Output)
		}
		sub := harness.Prompt(fmt.Sprintf("Request:\n%s\n\nIndependent answers:\n\n%sCombine them into one answer.",
			in.Text(), numbered("Answer", outs)))
		sub.RunID = s.fork("aggregate")
		agg, err := s.advance(p.Aggregator, sub)
		switch {
		case agg == nil:
			return "", err
		case agg.State == harness.StateSuspended:
			return s.park(agg.Waits...)
		case err != nil:
			return s.fail(err)
		case !agg.OK():
			return s.stoppedChild("aggregator", agg)
		}
		return s.ok(agg.Output)
	}
	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "## %s\n%s\n\n", harness.NameOf(p.Branches[i]), strings.TrimSpace(r.Output))
	}
	return s.ok(strings.TrimSpace(b.String()))
}

// fanOut runs runners concurrently with per-index inputs, keeping results in
// input order and isolating failures per branch. A slot holds a nil Run only if that
// branch's worker stopped, and then the error is a stop.
func fanOut(s *scope, runners []harness.Runner, input func(i int) harness.Input, limit int) ([]*harness.Run, []error) {
	results := make([]*harness.Run, len(runners))
	errs := make([]error, len(runners))
	if limit <= 0 {
		limit = len(runners)
	}
	sem := make(chan struct{}, max(limit, 1))
	var wg sync.WaitGroup
	for i, r := range runners {
		in := input(i)
		in.RunID = s.fork(strconv.Itoa(i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.emit("fan-out", fmt.Sprintf("branch %d: %s", i+1, harness.NameOf(r)))
			results[i], errs[i] = s.advance(r, in)
		}()
	}
	wg.Wait()
	return results, errs
}
