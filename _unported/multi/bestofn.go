package multi

import (
	"context"
	"fmt"

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
// Score wins. Candidates that did not complete are not eligible.
type BestOfN struct {
	Name       string
	Candidates []harness.Runner
	Judge      harness.Runner
	Score      func(in harness.Input, r *harness.Result) float64
	// Criteria is added to the judge prompt.
	Criteria string
}

// RunnerName implements harness.Named.
func (b *BestOfN) RunnerName() string { return b.Name }

// Run implements harness.Runner.
func (b *BestOfN) Run(ctx context.Context, in harness.Input) (*harness.Result, error) {
	s := open(ctx, "best_of_n", b.Name)
	results, errs := fanOut(s, b.Candidates, func(int) harness.Input { return in }, 0)
	var eligible []int
	for i, r := range results {
		if errs[i] == nil && r.OK() {
			eligible = append(eligible, i)
		}
	}
	if len(eligible) == 0 {
		return s.done(in, "", harness.StopFailed, "no candidate completed", nil)
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
		s.emit("verdict", fmt.Sprintf("score picked candidate %d (%.2f)", winner+1, best), nil)
	case b.Judge != nil && len(eligible) > 1:
		outs := make([]string, len(eligible))
		for k, i := range eligible {
			outs[k] = results[i].Output
		}
		criteria := b.Criteria
		if criteria == "" {
			criteria = "correctness, completeness and clarity"
		}
		prompt := fmt.Sprintf("Task:\n%s\n\nCandidate answers:\n\n%sJudge them on %s. "+
			"Reply with one line 'WINNER: <number>' followed by a one-sentence reason.", in.Text(), numbered("Candidate", outs), criteria)
		verdict, err := s.run(b.Judge, harness.Prompt(prompt))
		if err != nil {
			return s.fail(in, fmt.Errorf("judge: %w", err))
		}
		if k, ok := parseWinner(verdict.Output, len(eligible)); ok {
			winner = eligible[k]
		}
		s.emit("verdict", verdict.Output, map[string]any{"winner": winner + 1})
	}
	return s.ok(in, results[winner].Output)
}
