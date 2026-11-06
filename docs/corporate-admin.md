# Fit with corporate-admin

corporate-admin's agent turn loop is hard-coded today. agent-harness is meant to replace it later. **That integration is out of scope for this repository.** This page records how the API was shaped to make it a mechanical change. It is based on corporate-admin at `ba00a904`.

## The loop today

`cmd/core/agent/turn/worker.go`, `run.loop`, is **event-sourced**. Each iteration reloads the turn from the log and asks a pure step function what to do next:

```go
switch s := agent.Next(r.c.Limits, turn).(type) {
case agent.CallModel:    // compact first if needed (then re-plan), else stream the provider
case agent.StartTool:    // record the start of a tool call
case agent.AskApproval:  // request a human decision; errAwaiting parks the turn
case agent.RunTool:      // execute
case agent.RecordResult: // append the tool result
case agent.Finish:       // final event (answer or failure)
case agent.Wait:         // awaiting approval without a decision → finish as failure
}
// a Refusal error finishes the turn with EvFail and the refusal reason
```

Because the next step is derived from the log, a turn survives process restarts at every step. agent-harness works the same way: `Fold` computes the run from a journal, and a pure `next` picks the step.

## Mapping

| corporate-admin | agent-harness |
|---|---|
| `agent.Next(limits, turn)` (pure function of the turn log) | `next` over `Fold(events)`; `harness.Transitions()` is the table, and `Run.Path` records the walk |
| `CallModel` (+ compaction) | `compose` (`Context`: `hooks/compact`; `BeforeModel`: profiles, budget pre-flight) → `model` (`WrapModel`, `AfterModel`) |
| `StartTool` | `tools`, per call |
| `AskApproval` | `BeforeTool` → `hooks/approval` (`Ask` opens a durable wait, `Approver` decides on the spot, `Record` audits) |
| `RunTool` | the `WrapTool` chain around `tool.Registry.Call` |
| a write tool lending the turn's transaction (`Tx.Lend`) | `tool.EffectAtomic` and `AtomicJournal.Atomically`: the tool's writes and `tool.done` commit together |
| `RecordResult` | `tool.done` in the journal, after `AfterTool` |
| `Finish` | `verify` (`CheckOutput`) → `closing` → `done` |
| `AskApproval` → `errAwaiting`, `Wait` | `suspended`: the run parks with `Run.Waits`, and any process resumes it with a `Signal` |
| a decision or a stop written by a handler | `Agent.Deliver`: `wait.resolved` or `run.stopping`, no claim, `Wake` now |
| a retry card after a crash during a send | `WaitUnknownOutcome`, answered with `Signal{Rerun: true}` (run again) or a `Value` (the result) |
| risk class and gate reason on the approval | `Decision.Tags`, recorded on `tool.gated` |
| the lease heartbeat on the turn row | `Renewer.Renew(runID, headSeq, until)`, refused once the head moved |
| `Refusal` | `harness.Stop(StopGuardrail \| StopPolicy, detail)` → `stopped` |
| limits | `hooks/budget.Limits` (+ `Agent.MaxTurns`) |
| a conversation's turns | a chain of runs, `Input.After` naming the previous turn; `HistoryReader` over the conversation |
| tenant and principal on the turn row | `Input.Meta`, recorded on `run.started` and read from `Run.Meta` on every resume |
| subject snapshot and memory in the prompt | `Input.Pinned`, sent on every request of the turn and never inherited |

## Types

| corporate-admin | agent-harness |
|---|---|
| `cmd/core/gateways/llm` `Provider.Complete(ctx, req) iter.Seq2[Event, error]` | `model.Provider`, same shape; events `TextDelta`, `ToolCallDelta`, `Finish` |
| sealed `System` / `User` / `Assistant` / `ToolResult` messages | `model.Message` with `Role` plus constructors `model.System/User/Assistant/ToolResult` |
| `Usage{Prompt, Cached, Completion, Reasoning}` | `model.Usage{Input, Cached, Output, Reasoning}` |
| `cmd/core/tools/registry.go` `Spec{Name, Description, Risk, Exposure, AppliesTo, CallerOwned, InputSchema, …}` | `tool.Spec{Name, Description, Parameters, Risk, Scope}` (`Exposure`/`AppliesTo` → a `profile` or `BeforeModel` hook that filters `req.Tools`; `CallerOwned` → `Scope` + `approval.Principal`) |

## Integration sketch (later)

1. Write an adapter `llm.Provider` → `model.Provider`, a thin type mapping because both stream `iter.Seq2`.
2. Write an adapter for the tool registry: each corporate-admin `Spec` becomes a `tool.Tool`, with `Risk` mapped onto `tool.Risk`.
3. Replace `run.loop` with `harness.Agent.Run` plus corporate-admin's own hooks: approval through the existing approval service, tracing and metrics as sinks.
4. Implement `harness.Journal` over the turn log, and run `journaltest.Run` against it before it holds a real turn. `journal/postgres` is a worked example. Project `Event.Wake` to find crashed turns.
