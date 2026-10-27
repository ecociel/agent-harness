# Sequential

```
in → step 1 → step 2 → … → out
```

A pipeline where each step's output becomes the next step's input. Use it when the work has a fixed order and each stage has a different job, such as plan → implement → verify.

```go
p := &multi.Sequential{Name: "pipeline", Steps: []harness.Runner{planner, writer, reviewer}}
res, err := p.Run(ctx, harness.Prompt("Explain the inner loop"))
```

- **Input to step i+1.** By default this is a new user prompt quoting the *original request* and the previous step's output. Override it with `Next func(original harness.Input, prev *harness.Result) harness.Input`.
- **Stop rule.** The pipeline stops at the first step that doesn't complete and returns that step's stop reason (`Detail` names the step).
- **Events.** `pattern` with `step` (`"2/3 writer"`) before each step.

**Sources:** ecociel/agents `cmd/patterns/sequential`; Cursor's subagents docs describe an orchestrator chaining planner → implementer → verifier; Dani Zhu's *programmed workflows* (code decides the order).

**Example:** `ExampleSequential` in `multi/example_test.go`.
