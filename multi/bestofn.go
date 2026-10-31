package multi

import (
	"context"
	"fmt"
	"time"

	"github.com/ecociel/agent-harness"
)

// BestOfN runs N candidates on the same task in parallel and lets a judge
// pick the best answer.
//
//	      ┌→ candidate 1 ─┐
//	in ───┼→ candidate 2 ─┼→ judge → winner
//	      └→ candidate 3 ─┘
//
// This is Cursor's "run the same prompt across multiple models and pick the
// best result". Candidates are usually different models or temperatures.
// The judge is either a Runner that answers "WINNER: <n>" (an LLM judge), or
// a Score function (a code judge: tests passed, length, a rubric); with both,
// Score wins. Candidates that did not complete are not eligible. A candidate that
// waits suspends the pattern once the others have run as far as they can.
type BestOfN struct {
	Name       string
	Candidates []harness.Runner
	Judge      harness.Runner
	// Score must be a pure function of its arguments: it runs again after a crash.
	Score func(in harness.Input, r *harness.Run) float64
	// Criteria is added to the judge prompt.
	Criteria string
	// Journal makes the pattern durable. Nil inherits, like Agent.Journal.
	Journal harness.Journal
	// Hooks attach to the pattern's own run. A Redactor here covers every part.
	Hooks []harness.Hook
	// Lease is the same as Parallel.Lease.
	Lease time.Duration
}

// RunnerName implements harness.Named.
func (b *BestOfN) RunnerName() string { return b.Name }

// Run implements harness.Runner.
func (b *BestOfN) Run(ctx context.Context, in harness.Input) (*harness.Run, error) {
	parts := positional(b.Candidates)
	if b.Judge != nil {
		parts = append(parts, part{"judge", b.Judge})
	}
	spec := pattern{kind: "best_of_n", name: b.Name, journal: b.Journal, hooks: b.Hooks, lease: b.Lease, parts: parts}
	return spec.run(ctx, in, b.body)
}

func (b *BestOfN) body(s *scope) (string, harness.Spend, error) {
	in := s.in
	results, errs := fanOut(s, numbers("", len(b.Candidates)), b.Candidates, func(int) harness.Input { return in }, 0)
	var eligible []int
	var waits []harness.Wait
	for i, r := range results {
		switch {
		case r == nil:
			return s.halt(errs[i])
		case r.State == harness.StateSuspended:
			waits = append(waits, r.Waits...)
		case errs[i] == nil && r.OK():
			eligible = append(eligible, i)
		}
	}
	if len(waits) > 0 {
		return s.park(waits...)
	}
	if len(eligible) == 0 {
		return s.fail(fmt.Errorf("no candidate completed"))
	}
	winner := eligible[0]
	switch {
	case b.Score != nil:
		best := -1e308
		for _, i := range eligible {
			if sc := b.Score(in, results[i]); sc > best {
				best, winner = sc, i
			}
		}
	case b.Judge != nil && len(eligible) > 1:
		outs := make([]string, len(eligible))
		for k, i := range eligible {
			outs[k] = results[i].Output
		}
		criteria := b.Criteria
		if criteria == "" {
			criteria = "correctness, completeness and clarity"
		}
		verdict, err := s.need("judge", b.Judge, s.at("judge", harness.Prompt(fmt.Sprintf("Task:\n%s\n\nCandidate answers:\n\n%sJudge them on %s. "+
			"Reply with one line 'WINNER: <number>' followed by a one-sentence reason.", in.Text(), numbered("Candidate", outs), criteria))))
		if err != nil {
			return s.exit(err)
		}
		if k, ok := parseWinner(verdict.Output, len(eligible)); ok {
			winner = eligible[k]
		}
	}
	s.emit("verdict", fmt.Sprintf("candidate %d", winner+1))
	return s.ok(results[winner].Output)
}
