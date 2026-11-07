# corporate-admin as a consumer

corporate-admin runs its chat agents on this library. This page is the worked example of a production consumer: what the application supplies for each concern of [the concern matrix](design/durable-core.md#concern-matrix), where that code lives, and which library units the integration produced. It describes corporate-admin at `f2a3091a` on its branch `agent-harness-integration`, where the harness engine runs beside the old turn loop behind the per-org flag `chat-tab-v2`. The application's own design and progress are in its `docs/agent-harness-integration.md`. Paths below are relative to its `cmd/core`.

## The shape

One run is one turn of a conversation. The run ID is `<org>/<turn>` (`agent.RunRef` in `domain/agent/run.go`), and a child run is `<org>/<turn>/<call key>`. The library has no tenant concept. The org travels in the ID prefix and in `Input.Meta`. The engine (`agent/engine`) owns no loop. It builds a `harness.Agent` per claim, runs it from a River job, and reaches a run from outside in three ways: a posted message starts a turn, a decided card or an OAuth callback answers a wait, and a stop cancels the run.

The journal is one SQL table, `agent_journal` (migration `0002_agent_harness`). The append that records an event writes, in the same transaction, every row the application derives from it: turn state, wake and head, taint, waits and cards, allowances, the usage ledger, delta cleanup, the next job, the successor turn, the memory extraction and a notify. A pure function (`persistence/agentjournal/plan.go`) computes the projection from the events, and `exec.go` writes it. So a derived row exists exactly when its event does.

## What the application supplies

| Concern | Library piece | What corporate-admin supplies | Where |
|---|---|---|---|
| Durability | `Journal`, `Event.Wake` | A SQL journal that passes `journaltest.Run`. A River sweep resumes due turns through `agentjournal.Journal.Due` | `persistence/agentjournal/agentjournal.go`, `agent/engine/worker.go` |
| Tenancy and identity | `Input.Meta`, `Run.Meta`, child IDs under their parent | `agent.RunScope` (org, principal, agent, conversation, subject, personality, prior taint) as Meta. Hooks and tools read it back with `agent.ScopeOf(r.Meta)`, so a resume on any replica acts for the run's owner. The journal derives the org of every statement from the run ID | `domain/agent/run.go`, `agent/engine/hooks.go` |
| Side-effect safety | `tool.EffectAtomic`, `AtomicJournal`, `Signal.Rerun` | `effectOf` maps the app's risk classes to effects: memory, write, consequential and admin tools are atomic. The `authority` middleware hands an atomic tool the unit's transaction (`agentjournal.TxFrom`) and checks again inside the unit that its allowance still holds. A retry card answers an unknown outcome with `Signal{Rerun: true}` | `agent/engine/tooladapter.go`, `agent/engine/hooks.go`, `persistence/agentjournal/agentjournal.go` |
| Sessions | `Input.After`, `HistoryReader`, `Input.Pinned` | The append of `run.ended` drains the conversation inbox into the successor turn, which starts with `Input.After` naming its predecessor. `agentjournal.Journal.History` reads a conversation's final history in one query. The opening pins the subject snapshot and ranked memory | `persistence/agentinbox`, `agent/engine/worker.go` |
| Suspension and approvals | `Await`, `Decision.Tags`, `Agent.Deliver` | The `gate` hook decides each call from gathered facts and opens an approval or connect card as a wait, with risk class and gate reason as tags. The append writes the card into `agent_wait` with the input's hash and a sealed preview. That row is the binding store: `Engine.Decide` validates a decision against it and delivers the signal with `Agent.Deliver`, retrying on `ErrFenced` | `agent/engine/gate.go`, `agent/engine/engine.go`, `persistence/agentjournal/plan.go` |
| Long steps | `Renewer` | `Renew` locks the turn row alone and moves its wake only while the head is unchanged | `persistence/agentjournal/agentjournal.go` |
| Streaming | delta events with `Turn` and `Attempt` | A sink buffers the root run's text and writes it every 50 ms into the unlogged table `agent_delta`. The SSE handler sends journal entries by sequence and appends the open calls' text. The append of `model.done` or `model.lost` deletes the call's rows | `agent/engine/deltas.go`, `handlers/chat/render_v2.go` |
| Redaction | `guard.Stream`, `guard.Pseudonymizer`, `guard.RevealPaths` | For an org that opted in, a ring of credential rules through `guard.Stream` and a pseudonymizer over the org's vault (`agentvault`). A tool declares the JSON paths it needs in clear (`tools.Def.Reveal`), and the reveal middleware puts the real values back in those paths only, just before the tool runs | `agent/redaction`, `agent/engine/privacy.go` |
| Context and memory | `Context`, `Input.Pinned`, `Run.Inherited` | Long-term memory is selected at the opening and pinned. The `compactor` Context hook summarises the inherited history, and every successor inherits the edit. The append of a completed run enqueues the extraction of new memory and an episode | `agent/engine/worker.go`, `agent/engine/compact.go`, `persistence/agentjournal/extract.go` |
| Token spend | `model.Prices`, `hooks/budget` | Prices for the ledger the append writes, per call and attempt. The `caps` hook stops a run once its user's day or its org's month is spent. `hooks/budget` limits one run | `persistence/agentjournal/plan.go`, `agent/engine/caps.go` |
| Retry | `Recover`, `model.ClassContent` | The `retrier` hook waits out rate limits or parks the run. The `attachments` hook answers a refused part by resending without it | `agent/engine/recover.go`, `agent/engine/attach.go` |
| Providers and parts | `model.Provider`, `model.Part`, `Agent.Resolve` | The app's own client as a `model.Provider` (`llm.Harness`), and a resolver that reads attachment bytes within the run's conversation | `gateways/llm/harness.go`, `agent/engine/worker.go` |
| Tracing | `trace.FromEvents`, `trace.ContentNone` | The turn timeline folds the journal into spans without content, next to the ledger rows | `domain/agent/timeline_journal.go` |
| Multi-agent | `multi.Subagent`, `ChildTool` | `consult_agent` runs another of the user's agents as a read-only child run in the same journal, billed to the turn. A stop of the turn ends a parked consultation | `agent/engine/consult.go`, migration `0005_child_runs` |
| Evaluation | `eval.Target`, `eval.Generic` | A `Target` over the real stack, app check kinds such as `tasks_created` and `mail_sent_to`, and the scenario files | `agenteval/target.go`, `agenteval/kinds.go` |

## Where the application departs from these docs

- **The opening is recorded, not computed per request.** The worker takes the snapshot, memory and tool list at the run's first claim and records them with `run.started` (`Pinned`, `Meta`) or freezes them on the turn row. The concern matrix suggests loading memory in a `Context` hook with `Edit.Source`. A pinned input makes a resumed request equal the first, and compaction cannot edit it.
- **Memory is saved by the append, not in `AfterRun`.** The extraction job is enqueued on the transaction of the append that records a completed `run.ended`, so it is enqueued once and never lost. `AfterRun` runs at least once, and nothing joins it to that transaction.
- **The library's `approval`, `compact` and `retry` hooks are not used.** The cards, the allowances, the fold summary from episodes and the park after a rate limit are application policy, so the engine has its own `gate`, `compactor` and `retrier` hooks. It does use `hooks/budget` and `guard`.
- **The credential ring is worn only by an org that pseudonymises.** The design wants it on every run. It waited for `guard.Stream` to keep the bytes of tool-call arguments, so that the registry can still refuse a duplicate key. Library unit L11 made that change, and the engine has not yet attached the ring to every run (`agent/engine/privacy.go`).
- **No metrics or trace sink is attached yet.** At `f2a3091a` the only sink is the delta sink. corporate-admin has no trace exporter. Its design feeds the `ca_agent_*` metric families from a sink that keeps every name its alerts read, and until that lands they come from the old loop (`agent/turn/metrics.go`).
- **The journal table is `agent_journal`.** The old loop keeps `agent_event` until the cutover.

## Library units of the integration

Each unit is general. None names corporate-admin.

| Unit | What it added |
|---|---|
| L1 | `Input.Meta`, `Input.Pinned`, `Input.After`, `Run.Inherited`, `harness.FoldOn`, `harness.Prior`, `HistoryReader`. `Session` chains runs and no longer holds a history |
| L2 | `Agent.Deliver` |
| L3 | `tool.EffectAtomic`, `harness.AtomicJournal`, `harness.ErrNotAtomic`, `postgres.TxFrom` |
| L4 | `Signal.Rerun`, `Decision.Tags`, `Event.Attempt` on deltas, `Event.FirstToken` |
| L5 | `harness.Renewer`, implemented by `journal/postgres` |
| L6 | `model.Part`, `model.Resolver`, `Agent.Resolve`, `model.ClassContent`, parts in both adapters, `openai.Provider.Files` |
| L7 | `guard.Stream`, `guard.Pseudonymizer`, `guard.Vault`, `guard.NewMemVault`, `guard.Reveal`, `guard.RevealPaths`, `guard.RevealText`, Swiss phone numbers in `guard.PII`, bounded `guard.PII` and `guard.Secrets` |
| L8 | `model.Price`, `model.Prices`, `budget.Budget.WithPrices`, token kinds in `hooks/metrics`, `trace.FromEvents`, `trace.WithContent` |
| L9 | `eval.Scenario`, `eval.Target`, `eval.Conversation`, `eval.Observe`, `eval.Kinds`, `eval.Generic`, `eval.Play`, `eval.RunScenarios`, `eval.SelfTest`, `eval.Script.Provider` |
| L10 | Child run IDs under their parent (`<parent>/<key>`, `<parent>/<name>/<key>`), and `tool.lost` carrying its unknown-outcome wait in `Event.Waits` |
| L11 | Tool-call arguments keep their bytes through `guard.Stream` and `guard.Pseudonymizer`, a smaller hold-back window for `guard.Secrets`, and the rule that a failure of class `internal` never reaches `Recover` |
