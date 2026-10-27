# Planner / worker / judge

```
          ┌──────── round r (fresh start) ─────────────────┐
goal ──▶  planner ──TASK──▶ worker ┐                        │
          │        ──TASK──▶ worker ├─handoffs─▶ judge ─────┼─▶ CONTINUE → round r+1
          │        ──PLAN──▶ sub-planner (recursive) ┘      │   DONE     → synthesise
          └─────────────────────────────────────────────────┘
```

The hierarchical system Cursor describes for long-running autonomous work. A planner owns the goal and does no work itself. Isolated workers each take one task in parallel and write a handoff report. A judge decides whether another round is needed.

```go
pw := &multi.PlannerWorker{Planner: planner, Worker: worker, Judge: judge, Synthesizer: writer,
    MaxRounds: 3, MaxTasks: 6, MaxDepth: 1, MaxConcurrent: 4}
```

- **Planner contract.** One task per line as `TASK: …`. A slice big enough to need its own owner is `PLAN: …` and gets a **sub-planner** with the same machinery, up to `MaxDepth`. `DONE` means nothing is left. Each round runs at most `MaxTasks` tasks and sub-plans; the rest are dropped.
- **Workers.** Each gets the goal (as context only) plus its task, runs isolated with no cross-talk, and ends with a handoff report covering what it did, findings and concerns. Reports go up to the planner as the next round's input.
- **Fresh starts.** Every round starts the planner again from the goal plus the handoffs, with no long chat history. This counters drift and tunnel vision, a lesson from Cursor's runs.
- **Judge.** Optional. After each round it answers `DONE` or `CONTINUE`. The loop also ends when the planner writes `DONE` or `MaxRounds` is reached.
- **Synthesizer.** Optional. It turns the reports into the final answer; without one, the output is the reports.
- **Events.** `plan` (tasks and sub-plans per round), `handoffs`, `judge`, `end`.

**Sources:** Cursor [*Scaling long-running autonomous coding*](https://cursor.com/blog/scaling-agents): flat coordination through a shared file with locks failed, while planners and workers with a judge deciding whether to continue, plus fresh starts, worked. Cursor [*Towards self-driving codebases*](https://cursor.com/blog/self-driving-codebases): a root planner, recursive sub-planners, isolated workers and handoff reports. Also Dani Zhu's *hierarchical delegation* and ecociel/agents `hierarchical` and `supervisor`.

**Example:** `ExamplePlannerWorker`.
