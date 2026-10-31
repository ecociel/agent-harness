// Package harness is a small, dependency-free base for building agents in Go: a
// model-plus-tools loop that is durable by construction, with plugin points where
// consumer detail attaches, and multi-agent patterns (package multi) built from
// the same pieces.
//
// # The idea
//
// The truth about a run is an append-only journal of events. Fold computes a Run
// from the journal, and a private pure function picks the next step from that Run.
// A thin shell does the step, records what happened and loops. Nothing else holds
// state, so a run survives a crash at every step, and resuming on another machine
// is the same code as continuing on this one.
//
// A run with no Journal uses this same path over a slice:
//
//	res, err := agent.Run(ctx, harness.Prompt("hello"))
//
// With a Journal, Run is idempotent on Input.RunID. The same call from any process
// starts, resumes or returns the run, and it is also how a signal is delivered:
//
//	res, err := agent.Run(ctx, harness.Resume(id, harness.Signal{Wait: w, Value: "yes"}))
//
// # The state machine
//
//	start ─▶ compose ─▶ model ─▶ gate ─▶ tools ─┐
//	            ▲          │       │       │    │
//	            └──────────┼───────┴───────┴────┘ one model/tool turn
//	            ▲          ▼
//	            └────── verify ─▶ closing ─▶ done | stopped | failed
//
//	gate, tools or a long backoff ─▶ suspended ─▶ gate | compose
//	every live state ─▶ closing, on a decided stop
//
// A Composite run is a run whose work is to run others, so body takes the place of
// the inner loop. It writes no model or tool event, and its answer goes through the
// same verify and closing:
//
//	start ─▶ body ─▶ verify ─▶ closing ─▶ done | stopped | failed
//	          ▲ │
//	          └─┴─▶ suspended, while a child waits
//
// The legal edges are data (Transitions), keyed by the event that takes them.
// Fold refuses anything else, so a journal cannot hold a walk the machine forbids.
//
// # Plugin points
//
//	State     Point          Decides                          Recorded as
//	any       Redact         what text may exist              every durable event
//	claim     BeforeRun      admission, revocation            run.stopping
//	compose   Context        durable history edits            history.edited
//	compose   BeforeModel    this request only                nothing
//	model     WrapModel      the wire stream                  nothing
//	model     Recover        retry, backoff, fallback         model.failed
//	gate      BeforeTool     allow, deny, halt, await         tool.gated
//	tools     WrapTool       how one call runs                tool.done
//	verify    CheckOutput    accept, rewrite, revise, block   output.checked
//	closing   AfterRun       saving memory, flushing          run.ended
//
// A hook is any value with a Name method that also implements one or more point
// interfaces. A hook returns a value and the shell records it, so Fold never runs
// a hook and a decision cannot come out differently on replay.
//
// # Observability
//
// The journal and the event stream are one vocabulary. Every durable event reaches
// the sinks on the context (WithEvents) after it is stored, along with ephemeral
// deltas and hook timings. Sub-agents inherit the sinks.
//
// # Providers
//
// A model.Provider streams events (iter.Seq2), the same shape as
// corporate-admin's llm.Provider. provider/fake is deterministic for tests;
// provider/openai and provider/anthropic talk to real APIs.
package harness
