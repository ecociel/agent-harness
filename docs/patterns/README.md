# Multi-agent patterns

Every pattern in package `multi` is a `harness.Runner`. A pattern takes Runners and is one, so patterns nest freely, and an `*harness.Agent` is the leaf.

```go
type Runner interface {
    Run(ctx context.Context, in harness.Input) (*harness.Result, error)
}
```

What all patterns share:

- **Results.** A pattern returns a `Result` like an agent: `Output`, `Stop`, `Detail` and the summed `Usage`, `Turns`, `ModelCalls` and `ToolCalls` of its children. A child that stops (budget, guardrail, …) stops the pattern with the same reason unless the pattern says otherwise.
- **Observability.** Each pattern emits `pattern` events (`start`, the steps of the pattern, `end`) and opens a `pattern <kind>` span. Children run with the pattern as their parent, so their events carry `Depth`/`ParentRun` and their spans nest.
- **Hooks.** Patterns have no hooks of their own. Every agent inside keeps its own `Hooks`, so guardrails, approvals and budgets apply to every model and tool call in the tree. A shared `*budget.Budget` caps the whole tree; `budget.Child` gives a sub-tree its own slice.
- **Text contracts.** Where a model's answer steers the pattern (`WINNER: n`, `TASK:`, `APPROVED`/`REVISE:`, a route label), the parsing is deliberately lenient and the fallback is safe: an unknown label goes to the fallback route, an unparsable verdict counts as not approved.
- **Examples.** `multi/example_test.go` has a runnable `Example…` for every pattern, using the fake provider. The web chat lets you run each one with the full hook stack.

| Pattern | Use when | Doc |
|---|---|---|
| Sequential | fixed stages with different jobs | [sequential.md](sequential.md) |
| Parallel | independent views of the same input | [parallel.md](parallel.md) |
| Best-of-N + judge | quality varies run to run; you can judge | [best-of-n.md](best-of-n.md) |
| Router | requests fall into distinct kinds | [router.md](router.md) |
| Subagent | the lead decides at runtime what to delegate, with clean contexts | [subagent.md](subagent.md) |
| Background subagents | delegation shouldn't block the lead | [background.md](background.md) |
| Planner / worker / judge | large goals, many tasks, long runs | [planner-worker.md](planner-worker.md) |
| Evaluator–optimizer | quality needs a model to judge and iterate | [evaluator-optimizer.md](evaluator-optimizer.md) |
| Handoff | the conversation should move to a specialist | [handoff.md](handoff.md) |
| Shared-state team | a small pool of peers working a queue | [team.md](team.md) |
| Debate | independent errors, checkable by argument | [debate.md](debate.md) |

## Where each pattern came from

| Pattern | Cursor (blog/docs) | X post (Dani Zhu, "4 main multi-agent orchestration patterns") | ecociel/agents · tessmir |
|---|---|---|---|
| Sequential | subagents docs: orchestrator planner → implementer → verifier | programmed workflows | `sequential` · `agent` pipelines |
| Parallel | best-practices: parallel agents (worktrees) | — | `parallel`, `concurrent` · `parallel` |
| Best-of-N + judge | best-practices: same prompt on multiple models, pick the best; "parallel agents, judge" | — | `ensemble` · `critic` |
| Router | — | programmed workflows | `routing` · `routing` (safe fallback) |
| Subagent | subagents docs: own context window, foreground | hierarchical delegation | `orchestrator` · — |
| Background | subagents docs: background subagents | hierarchical delegation | `suspended`/`durable` (async) · — |
| Planner / worker / judge | scaling-agents: planners, workers, judge, fresh starts; self-driving-codebases: root planner, recursive sub-planners, isolated workers, handoff reports | hierarchical delegation | `hierarchical`, `supervisor` · — |
| Evaluator–optimizer | — | — | `critic`, `verified` · `verified` |
| Handoff | — | programmed workflows | `handoff`, `escalated` · — |
| Shared-state team | scaling-agents: shared file with locks failed → optimistic concurrency | shared-state teams | `blackboard`, `contractnet` · — |
| Debate | — | — | `ensemble` · — |
| *(human orchestration)* | Agents window | human orchestration | — (the web chat's pattern switcher plus approvals; `Session`) |

About the source: the brief asked for a post by "Lauren Tun" on Cursor Cloud's multi-agent architectures. It couldn't be found (see the README's open questions). The Cursor column is from Cursor's own publications:

- [scaling-agents](https://cursor.com/blog/scaling-agents)
- [self-driving-codebases](https://cursor.com/blog/self-driving-codebases)
- [agent-best-practices](https://cursor.com/blog/agent-best-practices)
- [docs/subagents](https://cursor.com/docs/subagents)

The X column is the closest matching post found: https://x.com/danizhu/status/2109166908591268315.
