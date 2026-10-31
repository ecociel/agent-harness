// Command guard shows hooks/guard, the rule-based guardrails, at its three places:
// BeforeRun refuses an input, BeforeTool denies a call and CheckOutput asks for a
// revision. It gets one run per behaviour. To keep a value out of everything, use
// guard.Redactor instead: see examples/hooks/points/redact.
//
//	go run ./examples/hooks/builtin/guard
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/builtin/guard
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/hooks/guard"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func main() {
	fmt.Println("Watch three runs: the input is refused, a tool is denied, an answer is revised.")
	provider, modelName := demo.Provider(fake.Script(
		fake.Call("delete_user", map[string]int{"id": 42}),
		fake.Text("I am not allowed to delete users."),
		fake.Text("The staging server is at 10.0.0.7."),
		fake.Text("The staging server is on the internal network."),
	))
	deleteUser := tool.Func(tool.Spec{
		Name:        "delete_user",
		Description: "Delete a user by id.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer"}}}`),
	}, func(context.Context, json.RawMessage) (string, error) { return "deleted", nil })
	agent := &harness.Agent{
		Name:         "ops",
		Instructions: "Answer in one sentence. Staging is at 10.0.0.7 on the internal network.",
		Provider:     provider,
		Model:        modelName,
		Tools:        tool.NewRegistry(deleteUser),
		Hooks: []harness.Hook{&guard.Guard{
			InputDeny:  guard.Secrets(),
			ToolDeny:   []string{"delete_user"},
			OutputDeny: []guard.Rule{guard.R("ip-address", `\b\d{1,3}(\.\d{1,3}){3}\b`)},
		}},
	}
	ctx := demo.Context()
	demo.Run(ctx, agent, harness.Prompt("Log me in, my password: hunter2"))
	demo.Run(ctx, agent, harness.Prompt("Delete user 42."))
	demo.Run(ctx, agent, harness.Prompt("Where is the staging server?"))
}
