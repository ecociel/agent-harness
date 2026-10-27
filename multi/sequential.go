package multi

import (
	"context"
	"fmt"

	"github.com/ecociel/agent-harness"
)

// Sequential is a pipeline: each step's output becomes the next step's input.
//
//	in → step 1 → step 2 → … → out
//
// Use it when the work has a fixed order and each stage has a different job
// (plan → implement → verify). The pipeline stops at the first step that does
// not complete; a stopped step stops the pipeline with the same reason.
type Sequential struct {
	Name  string
	Steps []harness.Runner
	// Next builds the input for step i+1 from step i's result and the
	// pipeline's original input. Nil passes the previous output as a new
	// user prompt that also quotes the original request.
	Next func(original harness.Input, prev *harness.Result) harness.Input
}

// RunnerName implements harness.Named.
func (p *Sequential) RunnerName() string { return p.Name }

// Run implements harness.Runner.
func (p *Sequential) Run(ctx context.Context, in harness.Input) (*harness.Result, error) {
	s := open(ctx, "sequential", p.Name)
	cur := in
	var last *harness.Result
	for i, step := range p.Steps {
		s.emit("step", fmt.Sprintf("%d/%d %s", i+1, len(p.Steps), harness.NameOf(step)), nil)
		res, err := s.run(step, cur)
		if err != nil {
			return s.fail(in, fmt.Errorf("step %d (%s): %w", i+1, harness.NameOf(step), err))
		}
		if !res.OK() {
			return s.stopped(in, harness.NameOf(step), res)
		}
		last = res
		if p.Next != nil {
			cur = p.Next(in, res)
		} else {
			cur = harness.Prompt(fmt.Sprintf("Original request:\n%s\n\nOutput of the previous step (%s):\n%s",
				in.Text(), harness.NameOf(step), res.Output))
		}
	}
	if last == nil {
		return s.ok(in, "")
	}
	return s.ok(in, last.Output)
}
