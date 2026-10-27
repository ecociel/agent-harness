// Package hooks is the home of the built-in hooks. Each sub-package is one
// cross-cutting concern and depends only on the harness core:
//
//   - guard: input, tool-call and output guardrails (veto, revise, redact)
//   - budget: token, cost, wall-clock and tool-call limits; loop detection;
//     hierarchical budgets for sub-agents
//   - metrics: counters and latency histograms with an in-memory registry
//   - trace: OpenTelemetry-shaped spans (gen_ai semantic conventions) with a
//     dependency-free exporter interface; spans nest across sub-agents
//   - approval: security, policy and approval ("SPA"): caller scopes and a
//     human-approval gate for risky tools, with an audit trail
//   - compact: summarise or window old context before the model call
//   - offload: move oversized tool results out of the context window
//   - retry: retries with backoff and a fallback provider around the model
//   - profile: per-model profiles (instructions, tool variants)
//   - trajectory: record full runs for evals and replay
//   - logging: log/slog output for every plugin point
//
// Every hook is an ordinary value: construct it, put it in Agent.Hooks.
package hooks
