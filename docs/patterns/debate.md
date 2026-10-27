# Debate

```
round 1:  A₁   B₁   C₁          (independent)
round 2:  A₂ ← B₁,C₁ …          (each sees the others)
judge:    WINNER: n
```

Several agents answer independently. For a few rounds each one is then shown the others' answers so it can correct itself (or them). Finally a judge picks the winner.

```go
d := &multi.Debate{Debaters: []harness.Runner{a, b, c}, Rounds: 2, Judge: judge}
```

- **Rounds** include the first, independent one (default 2). Every round runs the debaters in parallel.
- **Judge.** Replies `WINNER: <n>` with one sentence of reasoning. Without a judge, the first debater's final answer is returned together with the others'.
- **Cost.** Debate trades tokens for robustness. Use it where independent errors are likely and can be checked by argument, not for routine requests.
- **Events.** `round`, `verdict`, `end`.

**Sources:** ecociel/agents `ensemble`; the multi-agent debate literature (Du et al., *Improving Factuality and Reasoning through Multiagent Debate*).

**Example:** `ExampleDebate`.
