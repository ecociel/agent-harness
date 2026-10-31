package multi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ecociel/agent-harness"
)

// Team is a pool of peers working through a task list. Nobody is in charge: members
// post results, may add follow-up tasks, and the run ends when the list is empty.
//
//	tasks ──round──▶ member A, member B, member A ──NEW TASK──▶ tasks ──round──▶ …
//
// The list is a pure function of the seed and the journals: each round runs every
// task nobody has run yet, and the follow-ups it reports, in task order, form the next
// round. A task goes to a member by its number, so a resume rebuilds the same list and
// the same assignments, where a race to claim tasks could not be replayed. Each member
// sees the list as it stood when its round began. A failed task is reported and does
// not stop the others, and a task that waits holds the next round.
//
// Cursor found that flat self-coordination makes agents risk-averse at scale, which
// is why PlannerWorker exists; Team is still the right shape for a small pool of
// peers working through a queue.
type Team struct {
	Name    string
	Members []harness.Runner
	// Seed returns the initial tasks; nil uses the request as one task. It must be a
	// pure function of its input: it runs again after a crash.
	Seed func(in harness.Input) []string
	// MaxTasks caps the total number of tasks, including added ones (default 12).
	MaxTasks int
	// Journal makes the pattern durable. Nil inherits, like Agent.Journal.
	Journal harness.Journal
	// Hooks attach to the pattern's own run. A Redactor here covers every part.
	Hooks []harness.Hook
	// Lease is the same as Parallel.Lease.
	Lease time.Duration
}

// RunnerName implements harness.Named.
func (tm *Team) RunnerName() string { return tm.Name }

// member picks a task's member by its position in the list.
func (tm *Team) member(task int) harness.Runner { return tm.Members[task%len(tm.Members)] }

// Run implements harness.Runner.
func (tm *Team) Run(ctx context.Context, in harness.Input) (*harness.Run, error) {
	if len(tm.Members) == 0 {
		return nil, errors.New("multi: a team needs members")
	}
	maxTasks := def(tm.MaxTasks, 12)
	parts := make([]part, maxTasks)
	for i := range parts {
		parts[i] = part{fmt.Sprintf("task/%d", i), tm.member(i)}
	}
	spec := pattern{kind: "team", name: tm.Name, journal: tm.Journal, hooks: tm.Hooks, lease: tm.Lease, parts: parts, roles: positional(tm.Members)}
	return spec.run(ctx, in, func(s *scope) (string, harness.Spend, error) { return tm.body(s, maxTasks) })
}

// task is one line of the list.
type task struct {
	text, status, member, result string
}

func (tm *Team) body(s *scope, maxTasks int) (string, harness.Spend, error) {
	seed := []string{s.in.Text()}
	if tm.Seed != nil {
		seed = tm.Seed(s.in)
	}
	var tasks []task
	for _, t := range seed[:min(len(seed), maxTasks)] {
		tasks = append(tasks, task{text: t, status: "open"})
	}
	goal := s.in.Text()
	for start := 0; start < len(tasks); {
		end := len(tasks)
		prompts, ids := make([]harness.Input, end-start), make([]string, end-start)
		runners := make([]harness.Runner, end-start)
		for i := start; i < end; i++ {
			ids[i-start] = fmt.Sprintf("task/%d", i)
			runners[i-start] = tm.member(i)
			prompts[i-start] = harness.Prompt(teamPrompt(goal, i, tasks))
		}
		results, errs := fanOut(s, ids, runners, func(i int) harness.Input { return prompts[i] }, len(tm.Members))
		var waits []harness.Wait
		var added []string
		for i, r := range results {
			t := &tasks[start+i]
			t.member = fmt.Sprintf("%s#%d", harness.NameOf(runners[i]), (start+i)%len(tm.Members)+1)
			switch {
			case r == nil:
				return s.halt(errs[i])
			case r.State == harness.StateSuspended:
				waits = append(waits, r.Waits...)
			case errs[i] != nil:
				t.status, t.result = "failed", errs[i].Error()
			case !r.OK():
				t.status, t.result = "failed", string(r.Stop)
			default:
				t.status, t.result = "done", r.Output
				added = append(added, newTasks(r.Output)...)
			}
		}
		if len(waits) > 0 {
			return s.park(waits...)
		}
		start = end
		for _, text := range added {
			if len(tasks) < maxTasks {
				tasks = append(tasks, task{text: text, status: "open"})
			}
		}
		s.emit("round", fmt.Sprintf("%d of %d tasks run", start, len(tasks)))
	}
	var b strings.Builder
	failed := 0
	for i, t := range tasks {
		if t.status == "failed" {
			failed++
		}
		fmt.Fprintf(&b, "## Task %d (%s, %s): %s\n%s\n\n", i+1, t.status, t.member, t.text, strings.TrimSpace(t.result))
	}
	out := strings.TrimSpace(b.String())
	if failed > 0 {
		return out, s.below, harness.Stop(harness.StopFailed, fmt.Sprintf("%d tasks failed", failed))
	}
	return s.ok(out)
}

func teamPrompt(goal string, i int, tasks []task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Team goal:\n%s\n\nYour task (#%d):\n%s\n\nBoard:\n", goal, i+1, tasks[i].text)
	for n, t := range tasks {
		fmt.Fprintf(&b, "- #%d [%s] %s\n", n+1, t.status, t.text)
	}
	b.WriteString("\nDo your task. If you find follow-up work not on the board, add lines 'NEW TASK: <task>'.")
	return b.String()
}

func newTasks(out string) []string {
	var ts []string
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if len(l) > 9 && strings.EqualFold(l[:9], "NEW TASK:") {
			ts = append(ts, strings.TrimSpace(l[9:]))
		}
	}
	return ts
}
