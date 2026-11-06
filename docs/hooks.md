# Hooks

A hook is a plain value that implements one or more of the interfaces below. Put it in `Agent.Hooks`. Hooks run in list order, and the first middleware is the outermost layer.

One rule makes hooks safe across a crash: **a hook only ever returns a value.** It gets a `Run` by value and can't change it. The machine records what the hook returned, and `Fold` reads the record, so a decision is never made differently on replay. A hook can run more than once if a crash lands before its result is recorded, so its own side effects must tolerate repeats. Test a hook with `hooktest.Frozen`, which fails if it writes through the `Run` it was given.

| Point | Runs | Returns | Typical use |
|---|---|---|---|
| `Redact` | on all text, innermost | text | keep a value out of the model, the tools, every hook and the journal |
| `BeforeRun` | each time a worker takes the run | error (`Stop` ends the run) | admission, input guardrails, revocation |
| `Context` | before every model call | an `Edit`, recorded | compaction, memory |
| `BeforeModel` | before every model call | changes one request, not recorded | retrieval, model profiles |
| `WrapModel` | around the provider stream | a provider | streaming guardrails, response caches |
| `AfterModel` | on the reply, before it is recorded | a reply (not its usage) | reply guardrails |
| `Recover` | after a failed model call | a `Recovery`, recorded | retries, a fallback model |
| `BeforeTool` | for each call | `Allow`, `Deny`, `Halt` or `Await`, recorded | policy, approval |
| `WrapTool` | around one execution | a tool function | offloading, a ledger lookup on `c.Key` |
| `AfterTool` | on a result, before it is recorded | text | result guardrails |
| `CheckOutput` | on the candidate answer | `Accept`, `Revise` or `Block`, recorded | output guardrails, verifiers |
| `AfterRun` | when the outcome is decided, at least once | nothing | saving memory, keyed on `r.ID` |

`Redact` is special. It is a ring, not a step: it wraps the provider and the tools directly, inside `WrapModel` and `WrapTool`, and every durable event passes through it again. A redactor that fails ends the run with the text discarded. Children inherit it. While one is attached, a reply reaches sinks only when complete, unless the redactor also implements `ModelMiddleware` to filter the stream.

A `BeforeTool` decision can carry `Tags`, free-form facts such as a risk class or a reason code. They are recorded on `tool.gated` and redacted, so a later fold or a page reads them from the journal without the hook. The model never sees them.

Retries belong in `Recover`, not `WrapModel`: an attempt inside middleware isn't journaled, so a crash forgets it.

[examples/hooks](../examples/hooks/README.md) has a small runnable program for every point and every package below.

## Answering a run without working on it

`Agent.Deliver(ctx, in)` records `in.Signals` and `in.Cancel` and returns. It claims nothing, runs no hook except the redactors, and calls no model or tool. An HTTP handler that takes a decision, or an OAuth callback, uses it and leaves the work to a worker. A signal for no open wait, or an expired one, is `ErrStaleSignal`, and a repeat is ignored. When it resolves the waits of a parked run, or records a cancel, the last event's `Wake` is now, so a projection of `Wake` hands it to a worker at once. A signal to a run that is not parked leaves `Wake` a lease away, because its worker may still be in a step. A cancel of a running run fences its worker. If the worker appended first, `Deliver` returns `ErrFenced`, and sending it again is safe.

## A tool's writes in the journal's transaction

A tool with `Effect: tool.EffectAtomic` needs an `AtomicJournal`, or `Run` returns `ErrNotAtomic`. The machine records `tool.started` alone, then calls `Atomically(ctx, runID, fn)`. Inside `fn` run the `WrapTool` chain, the redactors, the tool and the `AfterTool` hooks, and `fn` returns the call's `tool.done`. The store appends it in the same unit as the tool's writes. A tool error is still a result: the store undoes the writes and records the error text. A tool whose statement fails must return the error, because on PostgreSQL the transaction is aborted and an "ok" result could not be appended. `WrapTool` and `AfterTool` hooks of an atomic call therefore run inside the unit, so keep them short.

## Long steps

A model call or a tool that may take longer than `Agent.Lease` would be swept as a crash. Renewal covers those two steps only, so a slow hook, such as a summariser in `Context`, still needs a lease above its duration. A journal that implements `Renewer` avoids that: every `Lease/3` during a step the worker calls `Renew(ctx, runID, headSeq, until)`, and the store moves the run's wake only while `headSeq` is still its last position. A refused renewal returns `ErrFenced`, and the worker stops at its next append. `journal/postgres` keeps `head_seq` on its run row for this.

## Sinks

Tracing, metrics and logging are not hooks. They are event sinks, attached once on the context, and every run in the tree reports to them:

```go
ctx = harness.WithEvents(ctx, trace.Sink(exporter))
ctx = harness.WithEvents(ctx, metrics.Sink(registry))
ctx = harness.WithEvents(ctx, logging.Sink(logger, slog.LevelDebug))
```

A sink gets each durable event once, after it is journaled, and only what has passed the redactors. It can miss the last event before a crash, so the journal is the source to reconcile a total from.

## Built in

| Package | Points | Concern |
|---|---|---|
| `hooks/guard` | `BeforeRun`, `BeforeTool`, `CheckOutput` | deny inputs, veto tools, revise or block outputs. `guard.Redactor` is the rule-based `Redact` ring. `Guard.Redact` only rewrites the final answer |
| `hooks/budget` | `BeforeRun`, `BeforeModel` | tokens, cost, model and tool calls, wall time, loop detection, read from the `Run`. `WithPool` applies limits to a tree or tenant, from the consumer's own projection |
| `hooks/approval` | `BeforeTool` | scopes, a risk threshold, an `Approver` or a durable wait (`Ask`), audit records |
| `hooks/compact` | `Context`, `Recover` | summarise old turns as a recorded edit, and answer a full context |
| `hooks/offload` | `WrapTool` | move large results to a `Store`, with a `read_offloaded` tool. A durable run needs a durable `Store` |
| `hooks/retry` | `Recover` | backoff and retries as recorded decisions, then one try on `FallbackModel`. `retry.Fallback` routes that name to another provider |
| `hooks/profile` | `BeforeModel` | per-model instructions, hidden tools, output caps |
| `hooks/trajectory` | `AfterRun` | full runs for evals. `trajectory.From` reads one from any `Run` |
| `hooks/trace` | sink | OpenTelemetry-shaped spans with stable IDs |
| `hooks/metrics` | sink | counters and latency histograms behind a `Recorder` |
| `hooks/logging` | sink | `log/slog` lines |

## Writing one

```go
type noFridays struct{}

func (noFridays) Name() string { return "no-fridays" }

func (noFridays) BeforeTool(_ context.Context, _ harness.Run, c harness.Call) harness.Decision {
	if c.Spec.Risk >= tool.RiskDangerous && time.Now().Weekday() == time.Friday {
		return harness.Deny("no dangerous changes on a Friday")
	}
	return harness.Allow()
}
```

Keep state in the journal. What a hook needs to know is in the `Run` it is given, so `hooks/budget` holds limits and no spend, and a budget survives a crash with nothing to restore. `harness.Note(ctx, ...)` attaches a remark to the hook's event.
