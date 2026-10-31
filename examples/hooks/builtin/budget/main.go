// Command budget shows hooks/budget. A Budget holds limits and no spend: it reads what
// was spent from the Run, which is the fold of the journal, so a budget survives a crash
// with nothing to restore. When a limit is reached, the next model call is refused and
// the run stops with budget_exhausted.
//
//	go run ./examples/hooks/builtin/budget
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/builtin/budget
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/hooks/budget"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func main() {
	fmt.Println("Watch the run stop before a third model call.")
	provider, modelName := demo.Provider(fake.Script(
		fake.Call("service_status", struct{}{}),
		fake.Call("service_status", struct{}{}),
	))
	status := tool.Func(tool.Spec{Name: "service_status", Description: "Current status of the service.", Effect: tool.EffectRead},
		func(context.Context, json.RawMessage) (string, error) { return "degraded", nil })
	limits := budget.New(budget.Limits{ModelCalls: 2})
	agent := &harness.Agent{
		Name:         "monitor",
		Instructions: "Call service_status until it reports ok.",
		Provider:     provider,
		Model:        modelName,
		Tools:        tool.NewRegistry(status),
		Hooks:        []harness.Hook{limits},
	}
	res := demo.Run(demo.Context(), agent, harness.Prompt("Wait for the service to recover."))
	spent := limits.Spent(*res)
	fmt.Printf("spent: %d model calls, %d tool calls\n", spent.ModelCalls, spent.ToolCalls)
}
