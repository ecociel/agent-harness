# Subagent (orchestrator–worker)

```
lead ──tool call──▶ subagent (fresh context) ──result──▶ lead
```

Turns a Runner into a **delegation tool**. The lead decides at runtime what to delegate by calling the tool with a self-contained task.

```go
lead, err := multi.WithSubagents(lead, multi.Subagent{
    Name: "researcher", Description: "Researches a question and reports in two sentences.", Agent: researcher,
})
```

- **Clean context.** As in Cursor's subagents, the sub-agent starts with only the task text the lead wrote. The lead must put everything needed into it, and the sub-agent's exploration never fills the lead's window.
- **Policy applies.** Delegation is an ordinary tool, so the lead's hooks govern it: guard can veto it, approval can gate it (`Risk`), budget counts it. The sub-agent's own hooks govern its own calls.
- **Depth cap.** `MaxDepth` (default 3) stops runaway recursion. Depth comes from the context, so it holds across patterns.
- **Stopped sub-agent.** The lead gets a tool result describing the stop and any partial output, and decides what to do.
- **Observability.** The sub-agent's events have `Depth+1` and `ParentRun` = the lead's run; its spans nest under the lead's `execute_tool` span.

**Sources:** Cursor [subagents docs](https://cursor.com/docs/subagents) (own context window; foreground); Dani Zhu's *hierarchical delegation* (Claude Code/Cursor subagents); ecociel/agents `orchestrator`.

**Example:** `ExampleSubagent`.
