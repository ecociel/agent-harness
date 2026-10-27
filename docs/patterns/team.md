# Shared-state team

```
member A ─┐  claim / post / add  ┌─ member B
          └──────▶  Board  ◀─────┘
```

A **shared-state team**: long-lived peers pull tasks from a `Board`, claim them with **optimistic concurrency**, post results and add follow-up tasks. Nobody is in charge. The run ends when the work list is empty and nobody is busy.

```go
tm := &multi.Team{Members: []harness.Runner{a, b}, MaxTasks: 12}
// or seed explicitly:
tm.Seed = func(in harness.Input) []string { return []string{"audit hooks", "audit patterns"} }
```

- **Board.** Each task has a version. `Claim(id, version, owner)` fails with `ErrConflict` if someone else changed it first, and the loser simply moves on. That is the optimistic-concurrency fix Cursor found after lock-based coordination through a shared file stalled. `Snapshot()` gives a consistent view and `Finish` posts a result. The board is safe for concurrent use and can be passed in (`Team.Board`) to inspect afterwards.
- **Follow-ups.** A member adds tasks by writing `NEW TASK: …` lines, capped by `MaxTasks` in total.
- **Waiting.** Idle members block until a task appears or everyone is done (`context.AfterFunc` wakes them on cancellation).
- **Result.** The output is all task results in order. Failed tasks give `StopFailed` with a count.
- **Events.** `claim`, `conflict`, `add`, `post`, `end`.

Cursor also found that flat self-coordination makes agents risk-averse at scale, which is why [PlannerWorker](planner-worker.md) exists. A Team is still the right shape for a small pool of peers working through a queue.

**Sources:** Dani Zhu's *shared-state teams*; Cursor [*Scaling agents*](https://cursor.com/blog/scaling-agents) (locks → optimistic concurrency); ecociel/agents `blackboard` and `contractnet`.

**Example:** `ExampleTeam`.
