// Command aftermodel shows the AfterModel point. A hook revises a reply before it is
// recorded. What it returns is what the journal, the history and every later hook get,
// and what it was given is gone. It cannot change Usage: the provider billed what it
// billed.
//
//	go run ./examples/hooks/points/aftermodel
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/points/aftermodel
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

type codename struct{ internal, public string }

func (codename) Name() string { return "codename" }

func (c codename) AfterModel(_ context.Context, _ harness.Run, resp model.Response) (model.Response, error) {
	fmt.Printf("[codename] the model said: %s\n", resp.Text)
	resp.Text = strings.ReplaceAll(resp.Text, c.internal, c.public)
	return resp, nil
}

func main() {
	fmt.Println("Watch the reply change before it is recorded.")
	provider, modelName := demo.Provider(fake.Script(fake.Text("Project Bluebird ships in May.")))
	agent := &harness.Agent{
		Name:         "press",
		Instructions: "Our next product is called Project Bluebird internally and ships in May.",
		Provider:     provider,
		Model:        modelName,
		Hooks:        []harness.Hook{codename{internal: "Project Bluebird", public: "our next product"}},
	}
	res := demo.Run(demo.Context(), agent, harness.Prompt("When does the next product ship?"))
	fmt.Printf("the history holds: %s\n", res.Messages[len(res.Messages)-1].Content)
}
