// Command beforemodel shows the BeforeModel point. It edits the request for one call,
// and nothing it adds is recorded. That suits retrieval: the passage reaches the model
// on this call, costs no tokens on later runs and is gone from the history afterwards.
//
//	go run ./examples/hooks/points/beforemodel
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/points/beforemodel
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
)

const passage = "Policy excerpt: refunds are possible within 30 days."

type retrieval struct{}

func (retrieval) Name() string { return "retrieval" }

func (retrieval) BeforeModel(_ context.Context, _ harness.Run, req *model.Request) error {
	fmt.Printf("[retrieval] add to this request only: %s\n", passage)
	req.Messages = append(req.Messages, model.User(passage))
	return nil
}

func main() {
	fmt.Println("Watch the passage reach the model and stay out of the run's history.")
	provider, modelName := demo.Provider(fake.Script(fake.Text("Within 30 days.")))
	agent := &harness.Agent{
		Name:         "support",
		Instructions: "Answer from the policy excerpt.",
		Provider:     provider,
		Model:        modelName,
		Hooks:        []harness.Hook{retrieval{}},
	}
	res := demo.Run(demo.Context(), agent, harness.Prompt("How long do I have to ask for a refund?"))
	var recorded bool
	for _, m := range res.Messages {
		recorded = recorded || strings.Contains(m.Content, passage)
	}
	fmt.Printf("history has %d messages, passage recorded: %v\n", len(res.Messages), recorded)
}
