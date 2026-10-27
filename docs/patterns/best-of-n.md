# Best-of-N + judge

```
      ┌→ candidate 1 ─┐
in ───┼→ candidate 2 ─┼→ judge → winner
      └→ candidate 3 ─┘
```

Runs N candidates on the same task in parallel, and a judge picks the best answer. Candidates are usually different models, temperatures or prompts.

```go
b := &multi.BestOfN{Candidates: []harness.Runner{gpt, claude, local}, Judge: judge, Criteria: "correctness"}
```

- **LLM judge.** It receives the task and the numbered candidates and must reply with `WINNER: <n>` plus a reason. If the reply can't be parsed, the first completed candidate wins; the `verdict` event carries the judge's text and the chosen number. With a single eligible candidate the judge is skipped.
- **Code judge.** `Score func(in, res) float64` (tests passed, length, a rubric). If both are set, `Score` wins.
- **Eligibility.** Only completed candidates can win. If none completed, the result is `StopFailed` ("no candidate completed").
- **Events.** `fan-out` per candidate, `verdict`, `end`.

**Sources:** Cursor *"parallel agents, judge"*, and *Best practices*: run the same prompt across multiple models and pick the best result. tessmir `critic`; ecociel/agents `ensemble`.

**Example:** `ExampleBestOfN`.
