package multi

import (
	"context"
	"fmt"
	"strconv"

	"github.com/ecociel/agent-harness"
)

// Sequential is a pipeline: each step's output becomes the next step's input.
//
//	in → step 1 → step 2 → … → out
//
// Use it when the work has a fixed order and each stage has a different job
// (plan → implement → verify). The pipeline stops at the first step that does
// not complete; a stopped step stops the pipeline with the same reason, and a step
// that waits suspends it.
type Sequential struct {
	Name  string
	Steps []harness.Runner
	// Next builds the input for step i+1 from step i's run and the pipeline's
	// original input. It must be a pure function of those two: it runs again after a
	// crash. Nil passes the previous output as a new user prompt that also quotes
	// the original request.
	Next func(original harness.Input, prev *harness.Run) harness.Input
	// Journal makes the pipeline durable. Nil inherits, like Agent.Journal.
	Journal harness.Journal
	// Hooks attach to the pipeline's own run. A Redactor here covers every step.
	Hooks []harness.Hook
}

// RunnerName implements harness.Named.
func (p *Sequential) RunnerName() string { return p.Name }

// Run implements harness.Runner.
func (p *Sequential) Run(ctx context.Context, in harness.Input) (*harness.Run, error) {
	spec := pattern{kind: "sequential", name: p.Name, journal: p.Journal, hooks: p.Hooks, parts: names(p.Steps)}
	return spec.run(ctx, in, func(s *scope) (string, error) {
		cur, output := s.in, ""
		for i, step := range p.Steps {
			s.emit("step", fmt.Sprintf("%d/%d %s", i+1, len(p.Steps), harness.NameOf(step)))
			cur.RunID = s.fork(strconv.Itoa(i))
			res, err := s.advance(step, cur)
			switch {
			case res == nil:
				return "", err
			case res.State == harness.StateSuspended:
				return s.park(res.Waits...)
			case err != nil:
				return s.fail(fmt.Errorf("step %d (%s): %w", i+1, harness.NameOf(step), err))
			case !res.OK():
				return s.stoppedChild(harness.NameOf(step), res)
			}
			output = res.Output
			if p.Next != nil {
				cur = p.Next(s.in, res)
			} else {
				cur = harness.Prompt(fmt.Sprintf("Original request:\n%s\n\nOutput of the previous step (%s):\n%s",
					s.in.Text(), harness.NameOf(step), res.Output))
			}
		}
		return s.ok(output)
	})
}
