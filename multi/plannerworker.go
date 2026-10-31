package multi

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ecociel/agent-harness"
)

// PlannerWorker is the hierarchical planner / worker / judge system Cursor
// describes in "Scaling long-running autonomous coding" and "Towards
// self-driving codebases":
//
//		          ┌──────── round r (fresh start) ───────────────┐
//		goal ──▶  planner ──TASK──▶ worker ┐                      │
//		          │        ──TASK──▶ worker ├─handoffs─▶ judge ───┼─▶ CONTINUE → round r+1
//		          │        ──PLAN──▶ sub-planner (recursive) ┘    │   DONE     → synthesise
//		          └───────────────────────────────────────────────┘
//
//	  - The planner owns the whole goal, does no work itself, and writes one
//	    task per line as "TASK: …". A slice big enough to need its own owner is
//	    written "PLAN: …" and gets a sub-planner with the same machinery,
//	    up to MaxDepth. "DONE" means nothing is left.
//	  - Workers get one task each, run in isolation (no cross-talk) and in
//	    parallel, and end with a handoff report: what was done, findings,
//	    concerns, deviations.
//	  - Handoffs flow back up to the planner as its next round's input.
//	  - A judge, if set, decides after each round whether to continue.
//	  - Every round starts the planner fresh (goal + handoffs, no long chat
//	    history) to fight drift and tunnel vision.
//
// A plan is a child's recorded answer, so a resume rebuilds the same rounds from the
// journals and every task keeps its ID. A round finishes before the next begins: a
// task that waits holds the round, once the others have run as far as they can.
type PlannerWorker struct {
	Name    string
	Planner harness.Runner
	Worker  harness.Runner
	// Judge answers CONTINUE or DONE after each round. Optional.
	Judge harness.Runner
	// Synthesizer turns the handoffs into the final answer. Optional; the
	// default output is the handoff reports.
	Synthesizer harness.Runner
	// MaxRounds caps planning rounds (default 3).
	MaxRounds int
	// MaxTasks caps tasks per round (default 6).
	MaxTasks int
	// MaxDepth caps sub-planner recursion (default 1).
	MaxDepth int
	// MaxConcurrent bounds parallel workers per round (0 = unbounded).
	MaxConcurrent int
	// Journal makes the pattern durable. Nil inherits, like Agent.Journal.
	Journal harness.Journal
	// Hooks attach to the pattern's own run. A Redactor here covers every part.
	Hooks []harness.Hook
	// Lease is the same as Parallel.Lease.
	Lease time.Duration

	depth int
}

// RunnerName implements harness.Named.
func (p *PlannerWorker) RunnerName() string { return p.Name }

// sub is the planner for one slice of the goal.
func (p *PlannerWorker) sub() *PlannerWorker {
	sub := *p
	sub.Name, sub.depth, sub.MaxRounds = p.Name+"/sub", p.depth+1, 1
	sub.Journal, sub.Hooks, sub.Lease = nil, nil, 0
	return &sub
}

func (p *PlannerWorker) deepest() int { return def(p.MaxDepth, 1) }

// Run implements harness.Runner.
func (p *PlannerWorker) Run(ctx context.Context, in harness.Input) (*harness.Run, error) {
	rounds, maxTasks := def(p.MaxRounds, 3), def(p.MaxTasks, 6)
	roles := []part{{"plan", p.Planner}, {"work", p.Worker}}
	var parts []part
	for round := range rounds {
		parts = append(parts, part{fmt.Sprintf("plan/%d", round), p.Planner})
		for i := range maxTasks {
			parts = append(parts, part{fmt.Sprintf("work/%d/%d", round, i), p.Worker})
			if p.depth < p.deepest() {
				parts = append(parts, part{fmt.Sprintf("sub/%d/%d", round, i), p.sub()})
			}
		}
		if p.Judge != nil {
			parts = append(parts, part{fmt.Sprintf("judge/%d", round), p.Judge})
		}
	}
	if p.Judge != nil {
		roles = append(roles, part{"judge", p.Judge})
	}
	if p.Synthesizer != nil {
		roles = append(roles, part{"synthesize", p.Synthesizer})
		parts = append(parts, part{"synthesize", p.Synthesizer})
	}
	spec := pattern{kind: "planner_worker", name: p.Name, journal: p.Journal, hooks: p.Hooks, lease: p.Lease, parts: parts, roles: roles}
	return spec.run(ctx, in, func(s *scope) (string, harness.Spend, error) { return p.body(s, rounds, maxTasks) })
}

func (p *PlannerWorker) body(s *scope, rounds, maxTasks int) (string, harness.Spend, error) {
	goal := s.in.Text()
	var handoffs []string
	for round := range rounds {
		plan, err := s.need("planner", p.Planner, s.at(fmt.Sprintf("plan/%d", round), harness.Prompt(planPrompt(goal, handoffs))))
		if err != nil {
			return s.exit(err)
		}
		tasks, plans, done := parsePlan(plan.Output)
		s.emit("plan", fmt.Sprintf("round %d: %d tasks, %d sub-plans", round+1, len(tasks), len(plans)))
		var ids []string
		var runners []harness.Runner
		var inputs []harness.Input
		for _, t := range plans {
			if p.depth < p.deepest() {
				ids = append(ids, fmt.Sprintf("sub/%d/%d", round, len(runners)))
				runners = append(runners, p.sub())
				inputs = append(inputs, harness.Prompt(t))
			} else {
				tasks = append(tasks, t)
			}
		}
		for _, t := range tasks {
			ids = append(ids, fmt.Sprintf("work/%d/%d", round, len(runners)))
			runners = append(runners, p.Worker)
			inputs = append(inputs, harness.Prompt(workerPrompt(goal, t)))
		}
		if len(runners) > maxTasks {
			ids, runners, inputs = ids[:maxTasks], runners[:maxTasks], inputs[:maxTasks]
		}
		if len(runners) == 0 {
			break
		}
		results, errs := fanOut(s, ids, runners, func(i int) harness.Input { return inputs[i] }, p.MaxConcurrent)
		var waits []harness.Wait
		for i, r := range results {
			switch {
			case r == nil:
				return s.halt(errs[i])
			case r.State == harness.StateSuspended:
				waits = append(waits, r.Waits...)
			case errs[i] != nil:
				handoffs = append(handoffs, fmt.Sprintf("[failed] %s: %v", inputs[i].Text(), errs[i]))
			case !r.OK():
				handoffs = append(handoffs, fmt.Sprintf("[%s] %s", r.Stop, r.Output))
			default:
				handoffs = append(handoffs, r.Output)
			}
		}
		if len(waits) > 0 {
			return s.park(waits...)
		}
		s.emit("handoffs", fmt.Sprintf("round %d: %d reports", round+1, len(results)))
		if done {
			break
		}
		if p.Judge != nil {
			v, err := s.need("judge", p.Judge, s.at("judge/"+strconv.Itoa(round), harness.Prompt(fmt.Sprintf("Goal:\n%s\n\nHandoff reports:\n\n%s"+
				"Is the goal achieved? Answer DONE or CONTINUE, then one sentence why.", goal, numbered("Report", handoffs)))))
			if err != nil {
				return s.exit(err)
			}
			if strings.Contains(strings.ToUpper(v.Output), "DONE") {
				break
			}
		}
	}
	if p.Synthesizer != nil && len(handoffs) > 0 {
		out, err := s.need("synthesizer", p.Synthesizer, s.at("synthesize", harness.Prompt(fmt.Sprintf("Goal:\n%s\n\nWorker handoff reports:\n\n%s"+
			"Write the final answer for the user.", goal, numbered("Report", handoffs)))))
		if err != nil {
			return s.exit(err)
		}
		return s.ok(out.Output)
	}
	return s.ok(strings.TrimSpace(numbered("Handoff", handoffs)))
}

func planPrompt(goal string, handoffs []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Goal:\n%s\n\n", goal)
	if len(handoffs) > 0 {
		fmt.Fprintf(&b, "Handoff reports from workers so far:\n\n%s", numbered("Report", handoffs))
	}
	b.WriteString("You are the planner. Do no work yourself. List the next tasks, one per line, as 'TASK: <task>'. " +
		"For a slice that needs its own planner write 'PLAN: <slice>'. If the goal is achieved write 'DONE'.")
	return b.String()
}

func workerPrompt(goal, task string) string {
	return fmt.Sprintf("Overall goal (context only):\n%s\n\nYour task:\n%s\n\n"+
		"Do the task. End with a handoff report: what you did, findings, concerns.", goal, task)
}

func parsePlan(out string) (tasks, plans []string, done bool) {
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-*0123456789. "))
		up := strings.ToUpper(l)
		switch {
		case strings.HasPrefix(up, "TASK:"):
			tasks = append(tasks, strings.TrimSpace(l[5:]))
		case strings.HasPrefix(up, "PLAN:"):
			plans = append(plans, strings.TrimSpace(l[5:]))
		case up == "DONE" || strings.HasPrefix(up, "DONE"):
			done = true
		}
	}
	return tasks, plans, done
}
