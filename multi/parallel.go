package multi

import (
	"context"
	"fmt"
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
type Parallel struct {
	Name     string
	Branches []harness.Runner
	// MaxConcurrent bounds concurrency (0 = all at once).
	MaxConcurrent int
	// Aggregator, if set, receives the original request plus all branch
	// outputs and produces the final answer.
	Aggregator harness.Runner
	// Join, if set, replaces both the default join and Aggregator.
	Join func(ctx context.Context, in harness.Input, results []*harness.Result) (string, error)
	// AllowPartial accepts results even if some branches did not complete.
	AllowPartial bool
}

// RunnerName implements harness.Named.
func (p *Parallel) RunnerName() string { return p.Name }

// Run implements harness.Runner.
func (p *Parallel) Run(ctx context.Context, in harness.Input) (*harness.Result, error) {
	s := open(ctx, "parallel", p.Name)
	results, errs := fanOut(s, p.Branches, func(int) harness.Input { return in }, p.MaxConcurrent)
	var failed []string
	for i, r := range results {
		switch {
		case errs[i] != nil:
			failed = append(failed, fmt.Sprintf("%s: %v", harness.NameOf(p.Branches[i]), errs[i]))
		case !r.OK():
			failed = append(failed, fmt.Sprintf("%s: %s", harness.NameOf(p.Branches[i]), r.Stop))
		}
	}
	if len(failed) > 0 && !p.AllowPartial {
		return s.done(in, "", harness.StopFailed, "branches did not complete: "+strings.Join(failed, "; "), nil)
	}
	s.emit("join", fmt.Sprintf("%d branches", len(results)), nil)
	switch {
	case p.Join != nil:
		out, err := p.Join(s.ctx, in, results)
		if err != nil {
			return s.fail(in, err)
		}
		return s.ok(in, out)
	case p.Aggregator != nil:
		var outs []string
		for _, r := range results {
			if r != nil {
				outs = append(outs, r.Output)
			}
		}
		agg, err := s.run(p.Aggregator, harness.Prompt(fmt.Sprintf(
			"Request:\n%s\n\nIndependent answers:\n\n%sCombine them into one answer.", in.Text(), numbered("Answer", outs))))
		if err != nil {
			return s.fail(in, err)
		}
		if !agg.OK() {
			return s.stopped(in, "aggregator", agg)
		}
		return s.ok(in, agg.Output)
	}
	var b strings.Builder
	for i, r := range results {
		if r == nil {
			continue
		}
		fmt.Fprintf(&b, "## %s\n%s\n\n", harness.NameOf(p.Branches[i]), strings.TrimSpace(r.Output))
	}
	return s.ok(in, strings.TrimSpace(b.String()))
}

// fanOut runs runners concurrently with per-index inputs, keeping results in
// input order and isolating failures per branch.
func fanOut(s *scope, runners []harness.Runner, input func(i int) harness.Input, limit int) ([]*harness.Result, []error) {
	results := make([]*harness.Result, len(runners))
	errs := make([]error, len(runners))
	if limit <= 0 {
		limit = len(runners)
	}
	sem := make(chan struct{}, max(limit, 1))
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for i, r := range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.emit("fan-out", fmt.Sprintf("branch %d: %s", i+1, harness.NameOf(r)), nil)
			res, err := r.Run(s.ctx, input(i))
			mu.Lock()
			results[i], errs[i] = res, err
			mu.Unlock()
			s.add(res)
		}()
	}
	wg.Wait()
	return results, errs
}
