// Command approval shows hooks/approval in its two modes. An Approver decides on the
// spot, here by amount. With Ask set the run suspends on a durable wait instead: nothing
// blocks, any process resumes it with a Signal, and the call is decided again against
// the answer. Suspending needs a Journal. Audit gets every decision.
//
//	go run ./examples/hooks/builtin/approval
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/builtin/approval
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/hooks/approval"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func audit(r approval.Record) {
	fmt.Printf("[audit] %s %s, rule %s, reason %q\n", r.Decision, r.Tool, r.Rule, r.Reason)
}

// underLimit says yes to refunds up to 100.
func underLimit(_ context.Context, req approval.Request) (bool, string) {
	var args struct {
		Amount int `json:"amount"`
	}
	if err := json.Unmarshal(req.Call.Arguments, &args); err != nil || args.Amount > 100 {
		return false, "refunds over 100 need a person"
	}
	return true, ""
}

func main() {
	fmt.Println("Watch an approver say yes and no, then a run suspend until a person answers.")
	provider, modelName := demo.Provider(fake.Script(
		fake.Call("refund", map[string]int{"amount": 50}),
		fake.Text("I refunded 50."),
		fake.Call("refund", map[string]int{"amount": 5000}),
		fake.Text("I could not refund 5000, that needs a person."),
		fake.Call("refund", map[string]int{"amount": 5000}),
		fake.Text("I refunded 5000."),
	))
	refund := tool.Func(tool.Spec{
		Name:        "refund",
		Description: "Refund an amount to the customer.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"amount":{"type":"integer"}}}`),
		Risk:        tool.RiskDangerous,
	}, func(context.Context, json.RawMessage) (string, error) { return "refunded", nil })
	ctx := demo.Context()

	onTheSpot := approval.New(tool.RiskWrite, approval.ApproverFunc(underLimit))
	onTheSpot.Audit = audit
	agent := &harness.Agent{
		Name:         "refunds",
		Instructions: "Use the refund tool, then report what happened.",
		Provider:     provider,
		Model:        modelName,
		Tools:        tool.NewRegistry(refund),
		Hooks:        []harness.Hook{onTheSpot},
	}
	demo.Run(ctx, agent, harness.Prompt("Refund 50."))
	demo.Run(ctx, agent, harness.Prompt("Refund 5000."))

	agent.Hooks = []harness.Hook{&approval.Policy{Threshold: tool.RiskWrite, Ask: true, Audit: audit}}
	agent.Journal = harness.NewMemJournal()
	res := demo.Run(ctx, agent, harness.Prompt("Refund 5000."))
	for _, w := range res.Waits {
		res = demo.Run(ctx, agent, harness.Resume(res.ID, harness.Signal{Wait: w.ID, Value: approval.Approved, By: "jan"}))
	}
}
