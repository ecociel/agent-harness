# Hooks

A hook is any value with a `Name() string` method that also implements one or more point interfaces. `Agent.Hooks` is an ordered slice. The machine indexes it once per run by type assertion (`harness.HookPoints(h)` reports which points a hook implements).

```go
type Hook interface{ Name() string }
```

## Point interfaces

```go
BeforeRunHook    BeforeRun(ctx, *Run) error
BeforeModelHook  BeforeModel(ctx, *Run, *model.Request) error
ModelMiddleware  WrapModel(*Run, next ModelCall) ModelCall
DeltaHook        OnDelta(ctx, *Run, model.Event)
AfterModelHook   AfterModel(ctx, *Run, *model.Response) error
BeforeToolHook   BeforeTool(ctx, *Run, model.ToolCall) Decision      // Allow, Deny(reason), Halt(reason)
ToolMiddleware   WrapTool(*Run, next ToolCallFunc) ToolCallFunc
AfterToolHook    AfterTool(ctx, *Run, ToolOutcome) error
AfterTurnHook    AfterTurn(ctx, *Run) error
OutputHook       CheckOutput(ctx, *Run, output string) Verdict       // Accept, Revise(fb), Block(msg), {Rewrite: s}
AfterRunHook     AfterRun(ctx, *Run, *Result)
TransitionHook   OnTransition(ctx, *Run, from, to State)
```

## Rules

- **Order.** Hooks run in slice order at every point. For middleware the first hook is the **outermost** layer: `[retry, trace]` means each retry attempt gets its own model span.
- **Errors.** A hook error at a point that returns `error` ends the run. `harness.Stop(reason, detail)` ends it on purpose (`stopped`, `err == nil` from `Run`); any other error fails it (`failed`).
- **Decisions and verdicts.** `BeforeTool` returns a `Decision`. The first non-allow decision wins. A denial is fed back to the model as a tool result (`{"denied":true,…}`) so it can adapt; a halt stops the run. `CheckOutput` returns a `Verdict`: `Revise` appends the feedback as a user message and loops back to `compose` (capped by `MaxRepairs`), `Block` stops, and `Rewrite` replaces the answer (redaction).
- **State.** Hooks run on the run's goroutine one at a time, so they may read and write the exported `Run` fields (for example `Messages` for compaction) without locking. `Run.Set` and `Run.Value` are scratch space shared between a hook's points and are safe from any goroutine.
- **Notes.** `run.Note(hookName, format, args…)` attaches a remark to the current invocation's `hook` event, such as "compacted 14 → 7 messages" or "offloaded 5321 bytes as off-1". The built-in hooks use notes, and the web chat shows them as tooltips in the hook matrix.
- **Shared hooks.** A hook value may be shared by many agents (all agents in a pattern, say). Keep per-run state in `Run.Set`, not in the hook. The built-in hooks are safe for concurrent use.

## Writing a hook

```go
type piiStream struct{}

func (piiStream) Name() string { return "pii-stream" }

// Streaming guardrail: stop the model mid-reply.
func (piiStream) OnDelta(ctx context.Context, r *harness.Run, ev model.Event) {
    if d, ok := ev.(model.TextDelta); ok && strings.Contains(d.Text, "756.") {
        r.Set("pii-stream.hit", true)
    }
}

func (piiStream) CheckOutput(_ context.Context, r *harness.Run, out string) harness.Verdict {
    if hit, _ := r.Value("pii-stream.hit").(bool); hit {
        return harness.Revise("do not include AHV numbers")
    }
    return harness.Accept()
}
```

## Built-in hooks and the video's middleware list

The video lists the middleware points a harness needs. Here is where each one lives in this library:

| Video | Point | Built-in |
|---|---|---|
| before the agent | `BeforeRun` | guard (input), budget (admission), trace (root span), trajectory |
| before each model call: summarisation, context assembly | `BeforeModel` | compact, profile, budget (pre-flight estimate) |
| wrap the model call | `WrapModel` | retry/fallback, trace, metrics |
| wrap each tool call: context offloading, approvals | `BeforeTool`, `WrapTool` | guard (veto), approval (SPA), profile (hidden tools); offload, trace |
| after the agent | `AfterRun` | trace, metrics, trajectory, logging |
| model profiles (per-model prompts and tool variants) | `BeforeModel`, `BeforeTool` | profile |
| traces, trajectories, tokens, latency | all | trace, trajectory, metrics, budget |
| evals (instruction, environment, verifier; LLM as judge) | — | package `eval`, `OutputHook` verifiers |

## Tracing without dependencies

`hooks/trace` produces OpenTelemetry-shaped spans: trace and span ids, parent ids, attributes and events. Names and attributes follow the GenAI semantic conventions (`invoke_agent <name>`, `chat <model>`, `execute_tool <name>`, `gen_ai.usage.input_tokens`, …). An `Exporter` receives finished `SpanData`. Exporters ship for memory (tests) and JSONL (files). To get real OTel, write a ~30-line exporter in your application that maps `SpanData` onto `go.opentelemetry.io/otel/trace`; that keeps the library dependency-free. The tracer comes from the hook (`trace.New(tracer)`) or, when that is nil, from the context (`trace.WithTracer`), so a server can trace each request separately. A tool span is placed on the context, so sub-agent spans nest under the tool call that started them.
