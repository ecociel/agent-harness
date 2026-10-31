// Package hooks is the home of the built-in hooks and sinks. Each sub-package is one
// cross-cutting concern and depends only on the harness core's exported API. State
// lives in the journal: a hook holds limits and rules, and reads the Run it is given.
//
//   - guard: input, tool-call and output guardrails, and a rule-based Redactor
//   - budget: token, cost, wall-clock and tool-call limits; loop detection
//   - approval: caller scopes and a human-approval gate for risky tools
//   - compact: summarise old context, durably, before the model call
//   - offload: move oversized tool results out of the context window
//   - retry: backoff, retries and a fallback model, as recorded decisions
//   - profile: per-model request changes (instructions, tool variants)
//   - trajectory: full runs for evals and replay, read from the events
//   - trace, metrics, logging: sinks, attached once with harness.WithEvents
//
// A hook is an ordinary value: construct it, put it in Agent.Hooks.
package hooks
