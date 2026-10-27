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

Because the next step is derived from the log, a turn survives process restarts at every step. agent-harness runs its machine in memory. The step vocabulary matches, but durability is the main gap (see below).

## Mapping

| corporate-admin | agent-harness |
|---|---|
| `agent.Next(limits, turn)` (pure function of the turn log) | the transition table plus the machine's step functions; `Result.Path` records the walk; the event stream (`WithEvents`) is the natural log to persist |
| `CallModel` (+ compaction) | `compose` (`BeforeModel`: `hooks/compact`, profiles, budget pre-flight) → `model` (`WrapModel`, `OnDelta`, `AfterModel`) |
| `StartTool` | `tools`, per call |
| `AskApproval` | `BeforeTool` → `hooks/approval` (`Approver` decides; `Record` audits) |
| `RunTool` | the `WrapTool` chain around `tool.Registry.Call` |
| `RecordResult` | the tool message appended in `tools`; `AfterTool`; `observe` (`AfterTurn`) |
| `Finish` | `route` → `verify` (`CheckOutput`) → `done` |
| `AskApproval` → `errAwaiting`, `Wait` | **gap**: approval blocks in-process (with a timeout). Parking a run needs a `suspended` state plus checkpoint/resume |
| `Refusal` | `harness.Stop(StopGuardrail \| StopPolicy, detail)` → `stopped` |
| limits | `hooks/budget.Limits` (+ `Agent.MaxTurns`) |

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
3. Replace `run.loop` with `harness.Agent.Run` plus corporate-admin's own hooks: approval via the existing approval service as an `approval.Approver`, persistence through `AfterTurn`/`AfterRun`, tracing through a `trace.Exporter` that bridges to its telemetry.
4. Add durability. Either persist the event stream and rebuild a `Run` from it (closest to today's design), or add a `suspended` terminal state with a resumable checkpoint (tessmir's `suspend` exit is the model for it). Today's `errAwaiting` approval flow needs one of the two.
