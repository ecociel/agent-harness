package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go/constant"
	"go/token"
	"go/types"
	"strings"
	"time"

	"github.com/ecociel/agent-harness/hooks/offload"
	"github.com/ecociel/agent-harness/tool"
)

// demoTools are the tools every demo agent can call. Each one exists to make
// a hook visible: clock and calculator are harmless reads, search_docs
// returns a large result (offload), send_email is dangerous and scoped
// (approval), delete_records is denied by the guardrail.
func demoTools(store *offload.Store) *tool.Registry {
	return tool.NewRegistry(
		tool.Func(tool.Spec{
			Name: "clock", Description: "Current date and time in Zurich.",
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		}, func(context.Context, json.RawMessage) (string, error) {
			loc, err := time.LoadLocation("Europe/Zurich")
			if err != nil {
				loc = time.FixedZone("CET", 3600)
			}
			return time.Now().In(loc).Format("Monday, 2 January 2006, 15:04 MST"), nil
		}),
		tool.Func(tool.Spec{
			Name: "calculator", Description: "Evaluate an arithmetic expression such as (17+4)*3.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"expression":{"type":"string"}},"required":["expression"]}`),
		}, calculate),
		tool.Func(tool.Spec{
			Name: "search_docs", Description: "Search the agent-harness documentation.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
		}, searchDocs),
		offload.Reader(store),
		tool.Func(tool.Spec{
			Name: "send_email", Description: "Send an e-mail on the user's behalf.", Risk: tool.RiskDangerous, Scope: "mail",
			Parameters: json.RawMessage(`{"type":"object","properties":{"to":{"type":"string"},"subject":{"type":"string"},"body":{"type":"string"}},"required":["to","subject"]}`),
		}, func(_ context.Context, raw json.RawMessage) (string, error) {
			var a struct{ To, Subject string }
			_ = json.Unmarshal(raw, &a)
			return fmt.Sprintf(`{"sent":true,"to":%q,"subject":%q,"note":"demo only, nothing left the machine"}`, a.To, a.Subject), nil
		}),
		tool.Func(tool.Spec{
			Name: "delete_records", Description: "Delete customer records.", Risk: tool.RiskDangerous,
			Parameters: json.RawMessage(`{"type":"object","properties":{"filter":{"type":"string"}}}`),
		}, func(context.Context, json.RawMessage) (string, error) {
			return `{"deleted":0}`, nil
		}),
	)
}

func calculate(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Expression string `json:"expression"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", err
	}
	if len(a.Expression) > 200 || strings.ContainsAny(a.Expression, "\"'`") {
		return "", fmt.Errorf("unsupported expression")
	}
	// go/types evaluates constant expressions: arithmetic only, no calls,
	// no identifiers, so it is safe for untrusted input.
	tv, err := types.Eval(token.NewFileSet(), nil, token.NoPos, a.Expression)
	if err != nil || tv.Value == nil {
		return "", fmt.Errorf("cannot evaluate %q", a.Expression)
	}
	v := tv.Value
	if v.Kind() == constant.Int || v.Kind() == constant.Float {
		f, _ := constant.Float64Val(constant.ToFloat(v))
		return fmt.Sprintf(`{"expression":%q,"value":%g}`, a.Expression, f), nil
	}
	return "", fmt.Errorf("not a number: %s", v)
}

// docs is a tiny corpus. search_docs deliberately returns a lot of text so
// the offload hook has something to move out of the context window.
var docs = []string{
	"The outer loop is one task or episode: start, prepare, then the inner loop, then verify, then done.",
	"The inner loop is one model/tool turn: compose, model, route, tools, observe, back to compose.",
	"BeforeModel hooks run in compose. Compaction (summarisation) and model profiles live there.",
	"WrapModel middleware wraps the provider call: retries, fallback providers, model spans, latency metrics.",
	"BeforeTool decides whether a tool call may run: guardrail veto, scopes, human approval.",
	"WrapTool middleware wraps one tool execution: tool spans, result offloading.",
	"CheckOutput runs in verify: output guardrails, redaction and verifiers; a revise verdict loops back to compose.",
	"The budget hook caps tokens, cost, model calls, tool calls and time, and detects repeated identical tool calls.",
	"Budgets are hierarchical: Child carves a sub-budget from the parent's remaining allowance.",
	"The trace hook records OpenTelemetry GenAI-style spans; sub-agents nest under the tool span that started them.",
	"Multi-agent patterns: sequential, parallel, best-of-n, router, subagent, background, planner-worker, evaluator-optimizer, handoff, team, debate.",
}

func searchDocs(_ context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal(raw, &a)
	q := strings.ToLower(a.Query)
	var b strings.Builder
	fmt.Fprintf(&b, "Results for %q:\n", a.Query)
	hits := 0
	for i, d := range docs {
		score := 0
		for _, w := range strings.Fields(q) {
			if len(w) > 2 && strings.Contains(strings.ToLower(d), w) {
				score++
			}
		}
		if score > 0 {
			hits++
			fmt.Fprintf(&b, "[%d] %s\n", i+1, d)
		}
	}
	if hits == 0 {
		b.WriteString("No direct hits; full index follows.\n")
	}
	// Append the full index so the result is large (offload demo).
	b.WriteString("\n--- full index ---\n")
	for i := 0; i < 3; i++ {
		for j, d := range docs {
			fmt.Fprintf(&b, "(%d.%d) %s\n", i+1, j+1, d)
		}
	}
	return b.String(), nil
}
