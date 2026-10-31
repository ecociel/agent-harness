// Command beforerun shows the BeforeRun point. It runs each time a worker takes a run,
// before any model call, so it is where admission lives. Returning harness.Stop ends
// the run on purpose: no tokens are spent and it does not count as a failure.
//
//	go run ./examples/hooks/points/beforerun
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/points/beforerun
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/provider/fake"
)

// admission lets a tenant run while it has quota. The tenant is the RunID's prefix.
type admission struct{ quota map[string]int }

func (admission) Name() string { return "admission" }

func (a admission) BeforeRun(_ context.Context, r harness.Run) error {
	tenant, _, _ := strings.Cut(r.ID, "/")
	if a.quota[tenant] == 0 {
		fmt.Printf("[admission] %s has no quota left, so stop before spending anything\n", tenant)
		return harness.Stop(harness.StopPolicy, tenant+" is over quota")
	}
	fmt.Printf("[admission] %s has quota %d, let the run in\n", tenant, a.quota[tenant])
	return nil
}

func main() {
	fmt.Println("Watch the second run: it stops before its first model call.")
	provider, modelName := demo.Provider(fake.Script(fake.Text("Bern.")))
	agent := &harness.Agent{
		Name:         "geo",
		Instructions: "Answer in one word.",
		Provider:     provider,
		Model:        modelName,
		Hooks:        []harness.Hook{admission{quota: map[string]int{"acme": 10, "globex": 0}}},
	}
	ctx := demo.Context()
	for _, id := range []string{"acme/1", "globex/1"} {
		in := harness.Prompt("What is the capital of Switzerland?")
		in.RunID = id
		demo.Run(ctx, agent, in)
	}
}
