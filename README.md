# agent-harness

A small Go library for production agents. A run is a **durable state machine**: an append-only journal of events is the truth, a pure function picks the next step, and a crash anywhere is recovered from the journal. Everything else is a **hook** at a named point, and **multi-agent patterns** are built from the same pieces. The core has no dependencies outside the standard library, and leaves the details to you: persistence, memory, prices, tracing backends, guardrail rules and redaction rules.

```
go get github.com/ecociel/agent-harness
```

> Private repository of écociel. No licence has been chosen yet.

- [Why](#why)
- [Architecture](#architecture)
- [Durability](#durability)
- [Hook points](#hook-points)
- [Multi-agent patterns](#multi-agent-patterns)
- [Try it: the web chat](#try-it-the-web-chat)
- [Learn the hooks: one example each](#learn-the-hooks-one-example-each)
- [Using it](#using-it)
- [What has not been verified](#what-has-not-been-verified)
- [Fit with corporate-admin](#fit-with-corporate-admin)
- [Sources](#sources)
- [Open questions](#open-questions)

## Why

An agent is a model, a harness and context. Every harness runs the same loop: call the model, run the tools it asks for, feed the results back, repeat. Production agents break in the moments around that loop: a process dies after a payment and before recording it, a person takes a day to approve something, a parallel branch crashes, a secret reaches a log, a budget is spent twice.

So this library makes those moments first-class:

- **Durable by construction.** `Run` is idempotent on `RunID`, so start, resume, signal and cancel are one call. A tool declares its `Effect`, and a crash mid-effect is repeated or put to a person, never guessed at.
- **Hooks return values.** A hook gets a `Run` by value, and what it returns is recorded. So hooks are off the replay path, and a decision can't come out differently after a crash.
- **One vocabulary.** The journal, the event sinks, `Fold`, a dashboard and an eval all read the same events.
- **Redaction is a ring, not a step.** The model, the tools, every hook and the journal only meet what the redactors left.
- **Patterns are runs.** Each is a composite run with its own journal, so a crash sweep can reach every position in a tree.

## Architecture

```
github.com/ecociel/agent-harness
├── harness (root)      Agent, Run, Fold, the state machine, Journal, hooks, events, Session
├── model               provider-neutral messages, tool calls, usage, streaming events, Provider
├── tool                tool specs (risk, scope, effect), Tool, Registry, Handoff
├── hooks/…             one package per concern (see docs/hooks.md)
├── hooktest            fails a test if a hook changes its Run
├── multi               multi-agent patterns (Runners built from Runners)
├── eval                tiny eval runner: tasks × checks (code or LLM judge)
├── journaltest         conformance suite for a Journal
├── journal/postgres    PostgreSQL Journal, in its own module
├── provider/fake       deterministic scripted provider for tests and demos
├── provider/openai     OpenAI-compatible Chat Completions (streaming)
├── provider/anthropic  Anthropic Messages API (streaming, thinking replay)
├── examples/webchat    web chat + live diagnostics (go run ./examples/webchat)
└── examples/hooks      one small program per hook (go run ./examples/hooks/tour)
```

Dependencies only point inwards: `hooks/*`, `multi` and `provider/*` depend on the core, and the core depends on nothing but `model` and `tool`.

## Durability

You implement two methods. A unique index on `(run_id, seq)` is the whole contract: it is the ownership fence, and the outbox if you project anything in the same transaction.

```go
type Journal interface {
	Append(ctx context.Context, runID string, evs []Event) error // all or nothing, ErrConflict if a position is taken
	Load(ctx context.Context, runID string) ([]Event, error)
}
```

`journal/postgres` is a complete one, including a sweep query and the optional `LineageReader`, which makes a cancel check cost one row instead of a parent's whole history. `journaltest.Run` checks any store against the contract, with crash sweeps through real agents.

```go
agent := &harness.Agent{Name: "refunds", Provider: p, Tools: tools, Journal: journal,
	Hooks: []harness.Hook{&approval.Policy{Threshold: tool.RiskDangerous, Ask: true}}}

in := harness.Prompt("Refund order 4711")
in.RunID = "refund-4711"
res, err := agent.Run(ctx, in)          // suspended: res.Waits says what it needs

// Any process, any time later.
res, err = agent.Run(ctx, harness.Resume("refund-4711",
	harness.Signal{Wait: res.Waits[0].ID, Value: approval.Approved, By: "jan"}))

// A sweep finds crashed runs, and any worker takes them.
ids, _ := journal.Due(ctx, time.Now())
for _, id := range ids {
	agent.Run(ctx, harness.Resume(id))
}
```

A tool's `Effect` says what a crash means for it. `EffectRead` and `EffectIdempotent` are repeated, with the same `tool.IdempotencyKey`. `EffectOnce`, the zero value, is never repeated blindly: an unknown outcome parks the run until a person answers.

The state machine is in [docs/state-machine.md](docs/state-machine.md), and the design, its evidence and its limits are in [docs/design/durable-core.md](docs/design/durable-core.md).

## Hook points

| Point | When | Use |
|---|---|---|
| `Redact` | on all text, innermost | keep a value out of everything |
| `BeforeRun` | each time a worker takes the run | admission, input guardrails |
| `Context` | before every model call, recorded | compaction, memory |
| `BeforeModel` | before every model call, one request only | retrieval, profiles |
| `WrapModel` | around the provider stream | streaming guardrails |
| `AfterModel` | on the reply, before it is recorded | reply guardrails |
| `Recover` | after a failed model call, recorded | retries, a fallback model |
| `BeforeTool` | for each call, recorded | policy, approval |
| `WrapTool` | around one execution | offloading |
| `AfterTool` | on a result, before it is recorded | result guardrails |
| `CheckOutput` | on the candidate answer, recorded | output guardrails, verifiers |
| `AfterRun` | when the outcome is decided, at least once | saving memory |

Built in: `guard`, `budget`, `approval`, `compact`, `offload`, `retry`, `profile`, `trajectory`, and the sinks `trace`, `metrics` and `logging`. See [docs/hooks.md](docs/hooks.md).

## Multi-agent patterns

All patterns are `harness.Runner`s, so they nest. Each is a durable composite run whose tree is a pure function of its input and its children's journals. Crash sweeps cover every one.

| Pattern | Type | Shape | Doc |
|---|---|---|---|
| Sequential | `multi.Sequential` | step → step → step | [doc](docs/patterns/sequential.md) |
| Parallel | `multi.Parallel` | branches in parallel, joined by code or an aggregator | [doc](docs/patterns/parallel.md) |
| Best-of-N + judge | `multi.BestOfN` | N candidates, an LLM or code judge picks | [doc](docs/patterns/best-of-n.md) |
| Router | `multi.Router` | classifier → one route, safe fallback | [doc](docs/patterns/router.md) |
| Subagent | `multi.Subagent` | a lead delegates through a tool, with a fresh context | [doc](docs/patterns/subagent.md) |
| Background | `multi.Background` | `start_<name>` returns a job, `collect` later | [doc](docs/patterns/background.md) |
| Planner / worker / judge | `multi.PlannerWorker` | tasks, recursive sub-planners, handoff reports, a fresh start per round | [doc](docs/patterns/planner-worker.md) |
| Evaluator–optimizer | `multi.EvaluatorOptimizer` | generator ⇄ evaluator | [doc](docs/patterns/evaluator-optimizer.md) |
| Handoff | `multi.Handoff` | peers transfer control, with a hop cap | [doc](docs/patterns/handoff.md) |
| Team | `multi.Team` | peers work through a task list in rounds and add follow-ups | [doc](docs/patterns/team.md) |
| Debate | `multi.Debate` | independent answers, cross-reading rounds, a judge | [doc](docs/patterns/debate.md) |

## Try it: the web chat

```sh
go run ./examples/webchat          # scripted demo model, no API key, works offline
open http://localhost:8080
```

The **chat** is on the left. The **diagnostics** on the right show, live for every turn: the state machine, a table of every run in the tree, a matrix of hooks × points, the budget, metrics, a span timeline and the event stream.

| Try | What fires |
|---|---|
| `What time is it?` | a full turn: tool call, then answer |
| `Search the docs for budget` | **offload** moves a large result out of the context |
| `Send an email to Jan` | **approval**: the run suspends, the stream ends, and your answer is a signal |
| `Delete all records` | **guard** vetoes the tool at `BeforeTool` |
| `My password is hunter2` | **guard** stops the run at `BeforeRun` |
| `Show my AHV number` | the **redactor** keeps it out of the journal too |
| `flaky: what time is it?` | **retry**: the first call fails, and the decision is recorded |
| `loop` | **budget** loop detection stops the run |
| `Tell me something long` (a few times) | **compact** records an edit of the history |

```sh
HARNESS_PROVIDER=openai    OPENAI_API_KEY=sk-…  [OPENAI_MODEL=gpt-4.1-mini]  go run ./examples/webchat
HARNESS_PROVIDER=openai    OPENAI_BASE_URL=http://localhost:11434/v1 OPENAI_MODEL=llama3.1  go run ./examples/webchat
HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=…  [ANTHROPIC_MODEL=claude-opus-5-5]  go run ./examples/webchat
```

The journal is in memory, so a restart forgets the runs. Flags: `-addr localhost:8080`, `-v`.

## Learn the hooks: one example each

[examples/hooks](examples/hooks/README.md) holds one small program for each of the 12 plugin points and each package under `hooks/`. Each prints a timeline of its run. They use a mock model and need no API key, and the same environment variables as the web chat switch any of them to a real model.

```sh
go run ./examples/hooks/tour                    # one run through all 12 points
go run ./examples/hooks/points/beforetool       # one point, in a hand-written hook
go run ./examples/hooks/builtin/approval        # one package from hooks/
```

## Using it

```go
weather := tool.Func(tool.Spec{Name: "weather", Description: "Weather for a city.", Effect: tool.EffectRead},
	func(ctx context.Context, args json.RawMessage) (string, error) { return `{"temp_c":11}`, nil })

a := &harness.Agent{
	Name:         "assistant",
	Instructions: "Answer briefly.",
	Provider:     anthropic.FromEnv(),
	Model:        "claude-opus-5-5",
	MaxTokens:    16_000,
	Tools:        tool.NewRegistry(weather),
	Hooks: []harness.Hook{
		guard.Redactor(guard.PII()),
		&guard.Guard{InputDeny: guard.Secrets()},
		budget.New(budget.Limits{Tokens: 50_000, RepeatCalls: 3}),
		&compact.Compactor{MaxTokens: 8_000},
		&retry.Retry{Attempts: 3},
	},
}
ctx = harness.WithEvents(ctx, metrics.Sink(registry))
res, err := a.Run(ctx, harness.Prompt("Weather in Zurich?"))
// res.Output, res.Stop, res.Path, res.Usage, res.Spent() …
```

- Chat: `harness.NewSession(runner).Send(ctx, text)` keeps history across runs, and is safe to repeat after a crash.
- Identity: `Input.Meta` (tenant, principal, labels) is recorded at start and read back from `Run.Meta`, so a resume by any process acts for the run's owner. A child started from a tool inherits it.
- Per-run context: `Input.Pinned` (a snapshot, retrieved memory) is recorded at start and sent on every request of that run, after the history and before the run's own messages. No other run inherits it, and a `Context` hook cannot edit it.
- Graceful shutdown: `ctx, drain := harness.WithDrain(ctx)`. Runs finish their step, record it and return `ErrDrained`.
- Patterns: see the `Example…` functions in `multi/example_test.go`.
- Evals: `eval.Run(ctx, runner, tasks, parallel)` with `eval.Contains`, `eval.StoppedWith` or `eval.Judge`.

Run the checks:

```sh
gofmt -l . && go vet ./... && go test -race ./...
# The real database: needs a Postgres, and fails in CI if the address is missing.
(cd journal/postgres && JOURNAL_POSTGRES_DSN=postgres://… go test -race ./...)
# The real API: costs money, so it needs saying twice.
AGENT_HARNESS_LIVE=1 ANTHROPIC_API_KEY=… go test ./provider -run Live -v
```

## What has not been verified

- **No adapter has met a real API.** They run through the machine against a scripted server that follows the published docs. `TestLiveToolLoop` checks a real one and has not been run.
- **The cases and their evidence** are listed in [the design](docs/design/durable-core.md#evidence), with what each one proves and what it doesn't.

## Fit with corporate-admin

The API is shaped so that corporate-admin's hard-coded loop (`cmd/core/agent/turn/worker.go`, `switch agent.Next(limits, turn)`) can later be replaced. That integration is out of scope here. The mapping is in [docs/corporate-admin.md](docs/corporate-admin.md). Both are event-sourced and pure in the same way, which was the point of the redesign.

## Sources

- **Harrison Chase (LangChain), "When to Build Your Own Agent Harness"** (Sequoia, https://youtu.be/HI2q3ci3Iuc). This is the primary source for the loop and the plugin points. Summary: [docs/sources/video-transcript-summary.md](docs/sources/video-transcript-summary.md).
- **Cursor**:
  - [Scaling long-running autonomous coding](https://cursor.com/blog/scaling-agents)
  - [Towards self-driving codebases](https://cursor.com/blog/self-driving-codebases)
  - [Best practices for coding with agents](https://cursor.com/blog/agent-best-practices)
  - [Subagents docs](https://cursor.com/docs/subagents)
- **X post on multi-agent architectures.** The brief asked for a post by "Lauren Tun". No such post was found (see [open questions](#open-questions)). The closest match is Dani Zhu, [*4 main multi-agent orchestration patterns*](https://x.com/danizhu/status/2109166908591268315) (11 Oct 2026): programmed workflows, hierarchical delegation, shared-state teams and human orchestration.
- **ecociel/agents**: the pattern catalogue and `production-agent-components.md` (inner and outer loop, explicit state machine), plus `budget`, `trace`, `checkpoint` and `eval`.
- **ecociel/tessmir** and **tessmir-old**: the callback set of the `agentloop` machine (trace, budget, checkpoint, redact, prompt-compose, tool authz/before/after, human gate), the stop reasons, and the pattern state-machine docs.

## Open questions

1. **"SPA".** The brief used the term without defining it. It is implemented as **S**ecurity · **P**olicy · **A**pproval: scopes, a risk threshold, a durable human gate and an audit trail in `hooks/approval`.
2. **The "Lauren Tun" X post** couldn't be found. The patterns come from Cursor's own blog and Dani Zhu's post.
3. **OpenTelemetry bridge.** The core stays dependency-free, so an OTel exporter (`trace.Exporter` → `go.opentelemetry.io/otel`) belongs in the app or a separate module.
4. **Licence.** None was added, as instructed.
