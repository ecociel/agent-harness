# Shared-state team

```
tasks ──round──▶ member A, member B, member A ──NEW TASK──▶ tasks ──round──▶ …
```

A **shared-state team**: peers work through a task list, post results and add follow-up tasks. Nobody is in charge. The run ends when the list is empty.

```go
tm := &multi.Team{Members: []harness.Runner{a, b}, MaxTasks: 12}
// or seed explicitly:
tm.Seed = func(in harness.Input) []string { return []string{"audit hooks", "audit patterns"} }
```

- **The list is a pure function.** It comes from the seed and the journals: each round runs every task nobody has run, and the follow-ups it reports, in task order, form the next round. So a resume rebuilds the same list.
- **Tasks go to members by number.** The first version raced members to claim tasks on a shared board. A race can't be replayed after a crash, so assignment is by position now. A slow member's task holds its round.
- **Follow-ups.** A member adds tasks by writing `NEW TASK: …` lines, capped by `MaxTasks` in total.
- **What a member sees.** The list as it stood when its round began, not its peers' live results.
- **Result.** The output is all task results in order. Failed tasks give `StopFailed` with a count, and don't stop the others.
- **Events.** `fan-out` and `round`.

Cursor found that flat self-coordination makes agents risk-averse at scale, which is why [PlannerWorker](planner-worker.md) exists. Team is still the right shape for a small pool of peers working through a queue.
