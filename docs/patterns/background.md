# Background subagents

```
lead ─start_researcher─▶ job-1 (running) …   lead keeps working …   lead ─collect(job-1, wait)─▶ result
```

Asynchronous delegation. `start_<name>` returns a job id immediately, and `collect` returns the job's status or, with `wait=true`, blocks until it finishes. The lead can start several jobs in one turn and keep working.

```go
bg := multi.NewBackground(multi.Subagent{Name: "researcher", Description: "…", Agent: researcher})
lead.Tools = tool.NewRegistry(append(myTools, bg.Tools()...)...)
defer bg.Wait() // in tests or on shutdown
```

- **Lifetime.** Jobs run with the context of the tool call that started them, minus its cancellation (`context.WithoutCancel`), so a job outlives the turn that started it but still carries events and tracing. `Wait` blocks until all jobs finish.
- **Same rules as Subagent.** Fresh context, depth cap, the lead's hooks gate `start_*`.
- **Events.** Each job is a `subagent` pattern scope (`start`, `delegate`, `end`), so the job's runs appear under the lead in the event stream even after the starting tool call has returned.

**Sources:** Cursor subagents docs (background subagents); ecociel/agents `suspended`/`durable` for the asynchronous shape.

**Example:** `ExampleBackground`.
