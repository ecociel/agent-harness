package multi

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

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
// are likely and checkable by argument. Without a Judge every final answer is
// returned. A round finishes before the next begins, so one debater that waits
// holds the debate at that round.
type Debate struct {
	Name     string
	Debaters []harness.Runner
	// Rounds including the first, independent one (default 2).
	Rounds int
	Judge  harness.Runner
	// Journal makes the pattern durable. Nil inherits, like Agent.Journal.
	Journal harness.Journal
	// Hooks attach to the pattern's own run. A Redactor here covers every part.
	Hooks []harness.Hook
	// Lease is the same as Parallel.Lease.
	Lease time.Duration
}

// RunnerName implements harness.Named.
func (d *Debate) RunnerName() string { return d.Name }

// Run implements harness.Runner.
func (d *Debate) Run(ctx context.Context, in harness.Input) (*harness.Run, error) {
	roles, rounds := positional(d.Debaters), def(d.Rounds, 2)
	var parts []part
	for round := range rounds {
		for i, r := range d.Debaters {
			parts = append(parts, part{fmt.Sprintf("%d/%d", round, i), r})
		}
	}
	if d.Judge != nil {
		roles = append(roles, part{"judge", d.Judge})
		parts = append(parts, part{"judge", d.Judge})
	}
	spec := pattern{kind: "debate", name: d.Name, journal: d.Journal, hooks: d.Hooks, lease: d.Lease, parts: parts, roles: roles}
	return spec.run(ctx, in, func(s *scope) (string, harness.Spend, error) { return d.body(s, rounds) })
}

func (d *Debate) body(s *scope, rounds int) (string, harness.Spend, error) {
	in := s.in
	answers := make([]string, len(d.Debaters))
	for round := range rounds {
		prev := append([]string(nil), answers...)
		ids := numbers(strconv.Itoa(round)+"/", len(d.Debaters))
		results, errs := fanOut(s, ids, d.Debaters, func(i int) harness.Input {
			if round == 0 {
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
		var waits []harness.Wait
		for i, r := range results {
			switch {
			case r == nil:
				return s.halt(errs[i])
			case r.State == harness.StateSuspended:
				waits = append(waits, r.Waits...)
			case errs[i] != nil:
				return s.fail(fmt.Errorf("debater %d: %w", i+1, errs[i]))
			case !r.OK():
				return s.stoppedChild(harness.NameOf(d.Debaters[i]), r)
			default:
				answers[i] = r.Output
			}
		}
		if len(waits) > 0 {
			return s.park(waits...)
		}
		s.emit("round", fmt.Sprintf("round %d complete", round+1))
	}
	if d.Judge == nil {
		return s.ok(strings.TrimSpace(numbered("Final answer", answers)))
	}
	v, err := s.need("judge", d.Judge, s.at("judge", harness.Prompt(fmt.Sprintf("Question:\n%s\n\nFinal answers after debate:\n\n%s"+
		"Reply 'WINNER: <number>' and one sentence why.", in.Text(), numbered("Answer", answers)))))
	if err != nil {
		return s.exit(err)
	}
	w, _ := parseWinner(v.Output, len(answers))
	s.emit("verdict", fmt.Sprintf("answer %d", w+1))
	return s.ok(answers[w])
}
