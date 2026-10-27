package multi

import (
	"context"
	"fmt"
	"strings"

	"github.com/ecociel/agent-harness"
)

// Debate has several agents answer independently, then show each of them
// the others' answers for a few rounds so they can correct themselves, and
// finally lets a judge pick the winner.
//
//	round 1:  A₁   B₁   C₁          (independent)
//	round 2:  A₂ ← B₁,C₁ …          (each sees the others)
//	judge:    WINNER: n
//
// Debate trades tokens for robustness on questions where independent errors
// are likely and checkable by argument. Without a Judge the first debater's
// final answer is returned together with the others'.
type Debate struct {
	Name     string
	Debaters []harness.Runner
	// Rounds including the first, independent one (default 2).
	Rounds int
	Judge  harness.Runner
}

// RunnerName implements harness.Named.
func (d *Debate) RunnerName() string { return d.Name }

// Run implements harness.Runner.
func (d *Debate) Run(ctx context.Context, in harness.Input) (*harness.Result, error) {
	s := open(ctx, "debate", d.Name)
	answers := make([]string, len(d.Debaters))
	rounds := def(d.Rounds, 2)
	for round := 1; round <= rounds; round++ {
		prev := append([]string(nil), answers...)
		results, errs := fanOut(s, d.Debaters, func(i int) harness.Input {
			if round == 1 {
				return in
			}
			var others []string
			for j, a := range prev {
				if j != i {
					others = append(others, a)
				}
			}
			return harness.Prompt(fmt.Sprintf("Question:\n%s\n\nYour previous answer:\n%s\n\nOther agents answered:\n\n%s"+
				"Reconsider. Point out errors (yours or theirs) and give your updated answer.", in.Text(), prev[i], numbered("Agent", others)))
		}, 0)
		for i, r := range results {
			switch {
			case errs[i] != nil:
				return s.fail(in, fmt.Errorf("debater %d: %w", i+1, errs[i]))
			case !r.OK():
				return s.stopped(in, harness.NameOf(d.Debaters[i]), r)
			}
			answers[i] = r.Output
		}
		s.emit("round", fmt.Sprintf("round %d complete", round), nil)
	}
	if d.Judge == nil {
		return s.ok(in, strings.TrimSpace(numbered("Final answer", answers)))
	}
	v, err := s.run(d.Judge, harness.Prompt(fmt.Sprintf("Question:\n%s\n\nFinal answers after debate:\n\n%s"+
		"Reply 'WINNER: <number>' and one sentence why.", in.Text(), numbered("Answer", answers))))
	if err != nil {
		return s.fail(in, fmt.Errorf("judge: %w", err))
	}
	w, _ := parseWinner(v.Output, len(answers))
	s.emit("verdict", v.Output, map[string]any{"winner": w + 1})
	return s.ok(in, answers[w])
}
