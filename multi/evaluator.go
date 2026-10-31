package multi

import (
	"context"
	"fmt"
	"strings"
	"time"

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
	// otherwise uses the text after "REVISE:" as feedback. It must be a pure function:
	// it runs again after a crash.
	Approve func(verdict string) (ok bool, feedback string)
	// Journal makes the pattern durable. Nil inherits, like Agent.Journal.
	Journal harness.Journal
	// Hooks attach to the pattern's own run. A Redactor here covers every part.
	Hooks []harness.Hook
	// Lease is the same as Parallel.Lease.
	Lease time.Duration
}

// RunnerName implements harness.Named.
func (e *EvaluatorOptimizer) RunnerName() string { return e.Name }

// Run implements harness.Runner.
func (e *EvaluatorOptimizer) Run(ctx context.Context, in harness.Input) (*harness.Run, error) {
	rounds := def(e.MaxRounds, 3)
	var parts []part
	for round := range rounds {
		parts = append(parts, part{fmt.Sprintf("generate/%d", round), e.Generator}, part{fmt.Sprintf("evaluate/%d", round), e.Evaluator})
	}
	spec := pattern{kind: "evaluator_optimizer", name: e.Name, journal: e.Journal, hooks: e.Hooks, lease: e.Lease, parts: parts,
		roles: []part{{"generate", e.Generator}, {"evaluate", e.Evaluator}}}
	return spec.run(ctx, in, func(s *scope) (string, harness.Spend, error) { return e.body(s, rounds) })
}

func (e *EvaluatorOptimizer) body(s *scope, rounds int) (string, harness.Spend, error) {
	in := s.in
	approve := e.Approve
	if approve == nil {
		approve = defaultApprove
	}
	gen, err := s.need("generator", e.Generator, s.at("generate/0", in))
	if err != nil {
		return s.exit(err)
	}
	draft, msgs := gen.Output, gen.Messages
	for round := 1; ; round++ {
		v, err := s.need("evaluator", e.Evaluator, s.at(fmt.Sprintf("evaluate/%d", round-1), harness.Prompt(fmt.Sprintf("Request:\n%s\n\nDraft:\n%s\n\n"+
			"Reply 'APPROVED' if the draft fully satisfies the request; otherwise 'REVISE: <specific feedback>'.", in.Text(), draft))))
		if err != nil {
			return s.exit(err)
		}
		ok, feedback := approve(v.Output)
		s.emit("verdict", fmt.Sprintf("round %d approved=%v", round, ok))
		if ok {
			return s.ok(draft)
		}
		if round >= rounds {
			return draft, s.below, harness.Stop(harness.StopMaxRepairs, fmt.Sprintf("not approved after %d rounds", rounds))
		}
		next := harness.Input{Messages: append(append([]model.Message(nil), msgs...),
			model.User("Reviewer feedback: "+feedback+"\nRevise your answer."))}
		if gen, err = s.need("generator", e.Generator, s.at(fmt.Sprintf("generate/%d", round), next)); err != nil {
			return s.exit(err)
		}
		if strings.TrimSpace(gen.Output) == strings.TrimSpace(draft) {
			return draft, s.below, harness.Stop(harness.StopLoop, "generator made no progress")
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
