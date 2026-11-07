# Hooks

A hook is a plain value that implements one or more of the interfaces below. Put it in `Agent.Hooks`. Hooks run in list order, and the first middleware is the outermost layer.

One rule makes hooks safe across a crash. **A hook only ever returns a value.** It gets a `Run` by value and can't change it. The machine records what the hook returned, and `Fold` reads the record, so a decision is never made differently on replay. A hook can run more than once if a crash lands before its result is recorded, so its own side effects must tolerate repeats. Test a hook with `hooktest.Frozen`, which fails if it writes through the `Run` it was given.

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

`Redact` is special. It is a ring, not a step. It wraps the provider and the tools directly, inside `WrapModel` and `WrapTool`, and every durable event passes through it again. A redactor that fails ends the run with the text discarded. Children inherit it. While one is attached, a reply reaches sinks only when complete, unless the redactor also implements `ModelMiddleware` to filter the stream.

A `BeforeTool` decision can carry `Tags`, free-form facts such as a risk class or a reason code. They are recorded on `tool.gated` with their values redacted, so a later fold or a page reads them from the journal without the hook. The model never sees them.

A message part passes the ring by its name and its text. Its bytes never do. `Agent.Resolve` adds them inside the ring, after every redactor, on the request the provider gets.

Retries belong in `Recover`, not `WrapModel`, because an attempt inside middleware isn't journaled and a crash forgets it. A failure of class `internal` (a hook, the journal or the harness itself failed, including a redactor that failed closed) is never offered to `Recover` and fails the run, because calling the model again cannot repair a local fault and must not undo a redactor's refusal.

[examples/hooks](../examples/hooks/README.md) has a small runnable program for every point and every package below.

## What a run carries for its hooks

`Run.Meta` is what `Input.Meta` recorded on `run.started`, such as the tenant and the principal that own the run. A hook or a tool reads identity from it, never from the context of whoever resumed the run. Its values are redacted like any text. Its keys are names and are not, and neither are the keys of `Tags`.

`Run.Messages` starts with the history a chained run inherited through `Input.After`, and `Run.Inherited` counts those messages. A `Context` edit may rewrite them, and every successor inherits the edit. `Run.Pinned` is not in `Messages`, so no edit reaches it. With `Pinned` set, an edit that spans inherited and own messages fails the run, so `hooks/compact` summarises the inherited part first and the run's own messages after that.

## Answering a run without working on it

`Agent.Deliver(ctx, in)` records `in.Signals` and `in.Cancel` and returns. It claims nothing, runs no hook except the redactors, and calls no model or tool. An HTTP handler that takes a decision, or an OAuth callback, uses it and leaves the work to a worker. A signal for no open wait, or an expired one, is `ErrStaleSignal`, and a repeat is ignored. When it resolves the waits of a parked run, or records a cancel, the last event's `Wake` is now, so a projection of `Wake` hands it to a worker at once. A signal to a run that is not parked leaves `Wake` a lease away, because its worker may still be in a step. A cancel of a running run fences its worker. If the worker appended first, `Deliver` returns `ErrFenced`, and sending it again is safe.

## A tool's writes in the journal's transaction

A tool with `Effect: tool.EffectAtomic` needs an `AtomicJournal`, or `Run` returns `ErrNotAtomic`. The machine records `tool.started` alone, then calls `Atomically(ctx, runID, fn)`. Inside `fn` run the `WrapTool` chain, the redactors, the tool and the `AfterTool` hooks, and `fn` returns the call's `tool.done`. The store appends it in the same unit as the tool's writes. A tool error is still a result. The store undoes the writes and records the error text. A tool whose statement fails must return the error, because on PostgreSQL the transaction is aborted and an "ok" result could not be appended. `WrapTool` and `AfterTool` hooks of an atomic call therefore run inside the unit, so keep them short.

## Long steps

A model call or a tool that may take longer than `Agent.Lease` would be swept as a crash. A journal that implements `Renewer` avoids that. Every `Lease/3` during a step, the worker calls `Renew(ctx, runID, headSeq, until)`, and the store moves the run's wake only while `headSeq` is still its last position. A refused renewal returns `ErrFenced`, and the worker stops at its next append. `journal/postgres` keeps `head_seq` on its run row for this. Renewal covers those two steps only, so a slow hook, such as a summariser in `Context`, still needs a lease above its duration.

## Sinks

Tracing, metrics and logging are not hooks. They are event sinks, attached once on the context, and every run in the tree reports to them:

```go
ctx = harness.WithEvents(ctx, trace.Sink(exporter))
ctx = harness.WithEvents(ctx, metrics.Sink(registry))
ctx = harness.WithEvents(ctx, logging.Sink(logger, slog.LevelDebug))
```

Spans carry structure, timing, tokens and outcomes, and no message text, tool arguments or tool results, so what an exporter sends out is never conversation content by default. `trace.WithContent(trace.ContentFull)` on `trace.SinkWith` or `trace.FromEvents` adds them.

A sink gets each durable event once, after it is journaled, and only what has passed the redactors. It can miss the last event before a crash, so the journal is the source to reconcile a total from.

Streamed text reaches sinks as delta events, which are not journaled. Each names its `Turn` and `Attempt`, so a sink can drop the text of an attempt that a retry replaced. `model.done` records `FirstToken`, the time from the request to the first streamed text or tool call.

## Built in

| Package | Points | Concern |
|---|---|---|
| `hooks/guard` | `BeforeRun`, `BeforeTool`, `CheckOutput`, `Redact`, `WrapModel`, `WrapTool` | deny inputs, veto tools, revise or block outputs, and the rule-based redaction ring. See [Redaction rules](#redaction-rules) |
| `hooks/budget` | `BeforeRun`, `BeforeModel` | tokens, cost, model and tool calls, wall time, loop detection, read from the `Run`. `WithPrices(model.Prices)` prices each model's calls, cached and reasoning tokens included, and a model with no entry bills zero. `WithPool` applies limits to a tree or tenant, from the consumer's own projection |
| `hooks/approval` | `BeforeTool` | scopes, a risk threshold, an `Approver` or a durable wait (`Ask`), audit records |
| `hooks/compact` | `Context`, `Recover` | summarise old turns as a recorded edit, and answer a full context |
| `hooks/offload` | `WrapTool` | move large results to a `Store`, with a `read_offloaded` tool. A durable run needs a durable `Store` |
| `hooks/retry` | `Recover` | backoff and retries as recorded decisions, then one try on `FallbackModel`. `retry.Fallback` routes that name to another provider |
| `hooks/profile` | `BeforeModel` | per-model instructions, hidden tools, output caps |
| `hooks/trajectory` | `AfterRun` | full runs for evals. `trajectory.From` reads one from any `Run` |
| `hooks/trace` | sink | OpenTelemetry-shaped spans with stable IDs for runs, model calls, tool calls and waits. `trace.FromEvents` builds the same spans offline from any journal |
| `hooks/metrics` | sink | counters and latency histograms behind a `Recorder`. Token counters carry `input`, `output`, `cached` and `reasoning` kinds |
| `hooks/logging` | sink | `log/slog` lines |

## Redaction rules

`guard.Redactor(rules)` is the rule-based `Redact` ring. It buffers the reply, so a sink sees the text only once the reply is complete. `Guard.Redact` only rewrites the final answer.

`guard.Stream(rules, window)` is the same ring, live. It holds back the last `window` bytes of the streamed text and releases what comes before them, redacted, so no delta carries part of a match. Every rule must have a bounded match (`{m,n}`, not `+` or `*`), and the window must hold the longest one. A window of 0 means the longest match. `PII()` and `Secrets()` are bounded. With a window of 0, `Secrets()` holds back 657 bytes and `PII()` holds back 345 bytes, so a reply shorter than the window streams nothing until it ends, in exchange for a match that never splits across two deltas.

Tool-call arguments are held until the end of the reply, and only their string values are rewritten, in place. Keys, key order, duplicate keys, white space and number formatting reach the tools as the model sent them, so a registry can still refuse an ambiguous call and an approval can hash the bytes that run.

`guard.Pseudonymizer` replaces each match with a stable token `⟦rule:hash⟧`, an HMAC of the value, and keeps the pair in a `guard.Vault`. The model, the journal and the sinks hold tokens only, and the same value gets the same token in every run. It streams and treats arguments like `Stream`. A vault failure fails the run closed. `guard.NewMemVault` is for tests and demos and is not durable.

`guard.Reveal(vault)` and `guard.RevealPaths(vault, paths)` are `WrapTool` middleware. They put real values back in a call's arguments just before the tool runs, everywhere or only in named JSON paths of named tools. They sit outside the ring, so the journal keeps the tokens and the ring tokenises the tool's output again. `guard.RevealText` is for a trusted reader outside a run.

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
