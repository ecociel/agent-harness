# Parallel (fan-out / fan-in)

```
      ┌→ branch 1 ─┐
in ───┼→ branch 2 ─┼→ join → out
      └→ branch 3 ─┘
```

Runs one input through several branches at once and joins the results. Each branch has its own context window, so a panel of specialists costs no more context per agent than one reviewer.

```go
p := &multi.Parallel{Name: "review", Branches: []harness.Runner{security, cost, facts}, MaxConcurrent: 3}
```

- **Join.** The default join is code: every branch must complete, and the outputs are concatenated under headers. Set `Aggregator` (a Runner) to have a model synthesise them, or `Join func(ctx, in, results) (string, error)` for full control.
- **Partial results.** `AllowPartial` accepts the branches that completed and ignores the rest.
- **Concurrency.** `MaxConcurrent` bounds it (0 means all at once). Branches share the context, so cancelling it cancels them all.
- **Events.** `fan-out` per branch, then `end`.

**Sources:** Cursor *Best practices for coding with agents* (run agents in parallel, each in its own worktree); ecociel/agents `parallel` and `concurrent`; tessmir `parallel`.

**Example:** `ExampleParallel`.
