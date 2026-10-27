# Router

```
in → classifier → label ─┬→ route A
                         ├→ route B
                         └→ fallback
```

A cheap classifier picks exactly one route from a closed set of labels, and only that route runs.

```go
r := &multi.Router{Classifier: cheap, Routes: map[string]harness.Runner{"billing": billing, "tech": tech}, Fallback: general}
```

- **Classifier prompt.** It lists the labels (`Labels()`) and asks for the label only. The answer is lower-cased and split into words, and the first word that is a known label wins, so "Label: billing." works.
- **Safe fallback.** An unknown label, a stopped classifier or a failed classifier all go to `Fallback`, so a request is never dropped. Without a fallback, an unroutable request ends with `StopFailed` ("unroutable request and no fallback").
- **Events.** `route` (label and target), `end`.

**Sources:** ecociel/agents `routing`; tessmir `routing` ("safe fallback"); Dani Zhu's *programmed workflows*.

**Example:** `ExampleRouter`.
