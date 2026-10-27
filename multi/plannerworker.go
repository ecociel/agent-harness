package multi

import (
	"context"
	"fmt"
	"strings"

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

	depth int
}

// RunnerName implements harness.Named.
func (p *PlannerWorker) RunnerName() string { return p.Name }

// Run implements harness.Runner.
func (p *PlannerWorker) Run(ctx context.Context, in harness.Input) (*harness.Result, error) {
	s := open(ctx, "planner_worker", p.Name)
	goal := in.Text()
	rounds, maxTasks, maxDepth := def(p.MaxRounds, 3), def(p.MaxTasks, 6), def(p.MaxDepth, 1)
	var handoffs []string
	for round := 1; round <= rounds; round++ {
		plan, err := s.run(p.Planner, harness.Prompt(planPrompt(goal, handoffs)))
		if err != nil {
			return s.fail(in, fmt.Errorf("planner: %w", err))
		}
		if !plan.OK() {
			return s.stopped(in, "planner", plan)
		}
		tasks, plans, done := parsePlan(plan.Output)
		s.emit("plan", fmt.Sprintf("round %d: %d tasks, %d sub-plans", round, len(tasks), len(plans)),
			map[string]any{"tasks": tasks, "plans": plans, "done": done})
		var runners []harness.Runner
		var inputs []harness.Input
		for _, t := range plans {
			if p.depth < maxDepth {
				sub := *p
				sub.Name, sub.depth, sub.MaxRounds = p.Name+"/sub", p.depth+1, 1
				runners = append(runners, &sub)
				inputs = append(inputs, harness.Prompt(t))
			} else {
				tasks = append(tasks, t)
			}
		}
		for _, t := range tasks {
			runners = append(runners, p.Worker)
			inputs = append(inputs, harness.Prompt(workerPrompt(goal, t)))
		}
		if len(runners) > maxTasks {
			runners, inputs = runners[:maxTasks], inputs[:maxTasks]
		}
		if len(runners) == 0 {
			break
		}
		results, errs := fanOut(s, runners, func(i int) harness.Input { return inputs[i] }, p.MaxConcurrent)
		for i, r := range results {
			switch {
			case errs[i] != nil:
				handoffs = append(handoffs, fmt.Sprintf("[failed] %s: %v", inputs[i].Text(), errs[i]))
			case !r.OK():
				handoffs = append(handoffs, fmt.Sprintf("[%s] %s", r.Stop, r.Output))
			default:
				handoffs = append(handoffs, r.Output)
			}
		}
		s.emit("handoffs", fmt.Sprintf("round %d: %d reports", round, len(results)), nil)
		if done {
			break
		}
		if p.Judge != nil {
			v, err := s.run(p.Judge, harness.Prompt(fmt.Sprintf("Goal:\n%s\n\nHandoff reports:\n\n%s"+
				"Is the goal achieved? Answer DONE or CONTINUE, then one sentence why.", goal, numbered("Report", handoffs))))
			if err != nil {
				return s.fail(in, fmt.Errorf("judge: %w", err))
			}
			cont := !strings.Contains(strings.ToUpper(v.Output), "DONE")
			s.emit("judge", v.Output, map[string]any{"continue": cont})
			if !cont {
				break
			}
		}
	}
	if p.Synthesizer != nil && len(handoffs) > 0 {
		out, err := s.run(p.Synthesizer, harness.Prompt(fmt.Sprintf("Goal:\n%s\n\nWorker handoff reports:\n\n%s"+
			"Write the final answer for the user.", goal, numbered("Report", handoffs))))
		if err != nil {
			return s.fail(in, err)
		}
		return s.ok(in, out.Output)
	}
	return s.ok(in, strings.TrimSpace(numbered("Handoff", handoffs)))
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

func def(v, d int) int {
	if v <= 0 {
		return d
	}
	return v
}
