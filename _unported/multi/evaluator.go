package multi

import (
	"context"
	"fmt"
	"strings"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
)

// EvaluatorOptimizer pairs a generator with an evaluator in a feedback loop.
//
//	in → generator → draft → evaluator ─APPROVED──▶ out
//	        ▲                    │
//	        └──── REVISE: … ─────┘
//
// The evaluator has its own context and sees only the request and the draft,
// so it is not anchored on the generator's reasoning. The stop rule is code:
// approved, round cap (StopMaxRepairs, best draft returned), or no progress
// (the generator returned the same draft twice: StopLoop).
//
// This is the multi-agent counterpart of the single-agent Verify → Compose
// repair edge: use OutputHook for cheap deterministic checks and this
// pattern when judging needs a model.
type EvaluatorOptimizer struct {
	Name      string
	Generator harness.Runner
	Evaluator harness.Runner
	// MaxRounds caps evaluations (default 3).
	MaxRounds int
	// Approve parses the evaluator's answer; nil looks for "APPROVED" and
	// otherwise uses the text after "REVISE:" as feedback.
	Approve func(verdict string) (ok bool, feedback string)
}

// RunnerName implements harness.Named.
func (e *EvaluatorOptimizer) RunnerName() string { return e.Name }

// Run implements harness.Runner.
func (e *EvaluatorOptimizer) Run(ctx context.Context, in harness.Input) (*harness.Result, error) {
	s := open(ctx, "evaluator_optimizer", e.Name)
	approve := e.Approve
	if approve == nil {
		approve = defaultApprove
	}
	gen, err := s.run(e.Generator, in)
	if err != nil {
		return s.fail(in, err)
	}
	if !gen.OK() {
		return s.stopped(in, "generator", gen)
	}
	draft, msgs := gen.Output, gen.Messages
	rounds := def(e.MaxRounds, 3)
	for round := 1; ; round++ {
		v, err := s.run(e.Evaluator, harness.Prompt(fmt.Sprintf("Request:\n%s\n\nDraft:\n%s\n\n"+
			"Reply 'APPROVED' if the draft fully satisfies the request; otherwise 'REVISE: <specific feedback>'.", in.Text(), draft)))
		if err != nil {
			return s.fail(in, fmt.Errorf("evaluator: %w", err))
		}
		ok, feedback := approve(v.Output)
		s.emit("verdict", v.Output, map[string]any{"round": round, "approved": ok})
		if ok {
			return s.ok(in, draft)
		}
		if round >= rounds {
			return s.done(in, draft, harness.StopMaxRepairs, "not approved after "+fmt.Sprint(rounds)+" rounds", nil)
		}
		next := harness.Input{Messages: append(append([]model.Message(nil), msgs...),
			model.User("Reviewer feedback: "+feedback+"\nRevise your answer."))}
		gen, err = s.run(e.Generator, next)
		if err != nil {
			return s.fail(in, err)
		}
		if !gen.OK() {
			return s.stopped(in, "generator", gen)
		}
		if strings.TrimSpace(gen.Output) == strings.TrimSpace(draft) {
			return s.done(in, draft, harness.StopLoop, "generator made no progress", nil)
		}
		draft, msgs = gen.Output, gen.Messages
	}
}

func defaultApprove(v string) (bool, string) {
	up := strings.ToUpper(v)
	if strings.Contains(up, "APPROVED") && !strings.Contains(up, "NOT APPROVED") {
		return true, ""
	}
	if i := strings.Index(up, "REVISE:"); i >= 0 {
		return false, strings.TrimSpace(v[i+len("REVISE:"):])
	}
	return false, strings.TrimSpace(v)
}
