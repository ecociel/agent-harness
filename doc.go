// Package harness is a small, dependency-free base for building agents in
// Go: a model-plus-tools loop driven by an explicit state machine, with
// plugin points where hooks attach, and multi-agent patterns (package multi)
// built from the same pieces.
//
// # The idea
//
// Almost every agent is "an LLM running in a loop calling tools". What makes
// harnesses differ is what they do at specific moments of that loop:
// summarise before the model call, wrap the model call in retries and spans,
// veto a tool call, offload a huge tool result, check the final answer. This
// package makes those moments explicit states and plugin points, so the core
// loop stays tiny and every concern is a hook you can read on its own.
//
// # The state machine
//
// The outer loop is one task (an episode). The inner loop is one model/tool
// turn, repeated until the model answers without asking for tools.
//
//	                 ┌─────────────── outer loop ────────────────┐
//	start ─▶ prepare ─▶ compose ─▶ model ─▶ route ─▶ verify ─▶ done
//	                      ▲                   │        │
//	                      │     inner loop    ▼        │ revise
//	                      └── observe ◀── tools        │ (repair)
//	                      ▲                            │
//	                      └────────────────────────────┘
//
//	every non-terminal state ─▶ stopped (on purpose) | failed (error)
//
// The legal edges are data (Transitions); the machine refuses anything else.
// Result.Path records the states a run visited.
//
// # Plugin points
//
//	State     Point          Typical hooks
//	prepare   BeforeRun      input guardrail, run span, budget admission
//	compose   BeforeModel    compaction/summarisation, model profiles, budget pre-flight
//	model     WrapModel      retry/fallback, model span, latency metrics
//	model     OnDelta        live streaming to a UI
//	model     AfterModel     token accounting, budget charge
//	tools     BeforeTool     guardrail veto, scopes, human approval
//	tools     WrapTool       tool span, result offloading, timeouts
//	tools     AfterTool      tool metrics, trajectory recording
//	observe   AfterTurn      loop detection, checkpoints
//	verify    CheckOutput    output guardrail, redaction, verifiers
//	terminal  AfterRun       close spans, run metrics, trajectories
//	any       OnTransition   state-level tracing
//
// A hook is any value with a Name method that also implements one or more
// of the point interfaces (BeforeRunHook, ModelMiddleware, …). Built-in hooks
// live under hooks/.
//
// # Observability
//
// Every transition and every hook invocation is emitted as an Event to the
// sinks on the context (WithEvents), including those of sub-agents, which
// carry their depth and parent run. The web chat example renders this stream
// live.
//
// # Providers
//
// A model.Provider streams events (iter.Seq2), the same shape as
// corporate-admin's llm.Provider. provider/fake is deterministic for tests;
// provider/openai and provider/anthropic talk to real APIs.
package harness
