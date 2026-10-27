# Evaluator–optimizer

```
in → generator → draft → evaluator ─APPROVED──▶ out
        ▲                    │
        └──── REVISE: … ─────┘
```

A generator and an evaluator in a feedback loop. The evaluator has its own context and sees only the request and the draft, so it isn't anchored on the generator's reasoning.

```go
eo := &multi.EvaluatorOptimizer{Generator: writer, Evaluator: critic, MaxRounds: 3}
```

- **Contract.** The evaluator replies `APPROVED`, or `REVISE: <specific feedback>`. The feedback goes back to the generator as `Reviewer feedback: … Revise your answer.` on top of its own conversation. Override parsing with `Approve func(verdict) (ok bool, feedback string)`.
- **Stop rules (code, not model).**
  - approved → `completed`
  - round cap → `max_repairs`, with the latest draft returned
  - no progress (same draft twice) → `loop_detected`
- **Single-agent counterpart.** An `OutputHook` that returns `Revise` takes the `verify → compose` repair edge inside one agent. Use that for cheap deterministic checks, and this pattern when judging needs a model.
- **Events.** `verdict` per round (with `round` and `approved`), `end`.

**Sources:** ecociel/agents `critic` and `verified`; tessmir `verified` (verification before acceptance); the evaluator–optimizer workflow from Anthropic's *Building effective agents*.

**Example:** `ExampleEvaluatorOptimizer`.
