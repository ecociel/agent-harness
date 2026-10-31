// Command aftertool shows the AfterTool point. A hook revises a result before it is
// recorded, so the journal and the model get what it returned. Here it masks a card
// number. A Redactor would do that for every text in the run. AfterTool is for one
// tool's result, with the Call to hand.
//
//	go run ./examples/hooks/points/aftertool
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/points/aftertool
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

var card = regexp.MustCompile(`\d{4} \d{4} \d{4} (\d{4})`)

type maskCard struct{}

func (maskCard) Name() string { return "mask-card" }

func (maskCard) AfterTool(_ context.Context, _ harness.Run, c harness.Call, content string) (string, error) {
	fmt.Printf("[mask-card] %s really returned: %s\n", c.Name, content)
	return card.ReplaceAllString(content, "**** **** **** $1"), nil
}

func main() {
	fmt.Println("Watch the tool's real result, then the masked one that gets recorded.")
	provider, modelName := demo.Provider(fake.Script(
		fake.Call("get_customer", map[string]int{"id": 42}),
		fake.Text("Customer 42 is Jan, paying with the card ending in 1111."),
	))
	lookup := tool.Func(tool.Spec{
		Name:        "get_customer",
		Description: "Look up a customer by id.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer"}}}`),
		Effect:      tool.EffectRead,
	}, func(context.Context, json.RawMessage) (string, error) {
		return `{"name":"Jan","card":"4111 1111 1111 1111"}`, nil
	})
	agent := &harness.Agent{
		Name:         "crm",
		Instructions: "Answer briefly from your tools.",
		Provider:     provider,
		Model:        modelName,
		Tools:        tool.NewRegistry(lookup),
		Hooks:        []harness.Hook{maskCard{}},
	}
	res := demo.Run(demo.Context(), agent, harness.Prompt("Who is customer 42 and how do they pay?"))
	for _, m := range res.Messages {
		if m.Role == model.RoleTool {
			fmt.Printf("the history holds: %s\n", m.Content)
		}
	}
}
