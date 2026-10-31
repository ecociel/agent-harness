// Command afterrun shows the AfterRun point. It runs when the outcome is decided and
// before the run is marked ended. It runs at least once: a crash can repeat it, so a
// write keys on r.ID and a repeat replaces itself. It returns nothing and cannot change
// the outcome.
//
//	go run ./examples/hooks/points/afterrun
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/points/afterrun
package main

import (
	"context"
	"fmt"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/provider/fake"
)

type summaries map[string]string

func (summaries) Name() string { return "summaries" }

func (s summaries) AfterRun(_ context.Context, r harness.Run) {
	s[r.ID] = fmt.Sprintf("%s, model calls %d, answer %q", r.Stop, r.ModelCalls, r.Output)
	fmt.Printf("[summaries] saved under %s\n", r.ID)
}

func main() {
	fmt.Println("Watch the summary get saved just before the run ends.")
	provider, modelName := demo.Provider(fake.Script(fake.Text("Bern.")))
	saved := summaries{}
	agent := &harness.Agent{
		Name:         "geo",
		Instructions: "Answer in one word.",
		Provider:     provider,
		Model:        modelName,
		Hooks:        []harness.Hook{saved},
	}
	res := demo.Run(demo.Context(), agent, harness.Prompt("What is the capital of Switzerland?"))
	fmt.Printf("saved[%s] = %s\n", res.ID, saved[res.ID])
}
