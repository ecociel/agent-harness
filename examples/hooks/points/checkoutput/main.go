// Command checkoutput shows the CheckOutput point. A hook judges the candidate answer.
// Revise sends the feedback to the model for another try, Accept lets the answer stand,
// and Block stops the run. The verdict is recorded. r.Repairs comes from the journal,
// so the hook needs no counter to revise only once.
//
//	go run ./examples/hooks/points/checkoutput
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/points/checkoutput
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/provider/fake"
)

type currency struct{}

func (currency) Name() string { return "currency" }

func (currency) CheckOutput(_ context.Context, r harness.Run, output string) harness.Verdict {
	if strings.Contains(output, "CHF") {
		fmt.Println("[currency] the answer names the currency")
		return harness.Accept()
	}
	if r.Repairs > 0 {
		fmt.Println("[currency] no currency, but it already had its one revision")
		return harness.Accept()
	}
	fmt.Println("[currency] no currency in the answer, ask again")
	return harness.Revise("State the price in CHF.")
}

func main() {
	fmt.Println("Watch the first answer come back for revision and the second get accepted.")
	provider, modelName := demo.Provider(fake.Script(fake.Text("The plan costs 30."), fake.Text("The plan costs 30 CHF.")))
	agent := &harness.Agent{
		Name:         "sales",
		Instructions: "The Basic plan costs 30 Swiss francs a month. Answer in one sentence.",
		Provider:     provider,
		Model:        modelName,
		Hooks:        []harness.Hook{currency{}},
	}
	res := demo.Run(demo.Context(), agent, harness.Prompt("How much is the Basic plan?"))
	fmt.Printf("repairs: %d\n", res.Repairs)
}
