// Command recover shows the Recover point. It decides what follows a failed model call,
// and the decision is recorded. Without a hook that retries, the first failure fails
// the run. demo.FailFirst injects the failures, so this behaves the same on a real
// model.
//
//	go run ./examples/hooks/points/recover
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/points/recover
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
)

type patient struct{ tries int }

func (patient) Name() string { return "patient" }

func (p patient) Recover(_ context.Context, _ harness.Run, f harness.Failure) harness.Recovery {
	if f.Class != model.ClassTransient || f.Attempt >= p.tries {
		fmt.Printf("[patient] %s on attempt %d, give up\n", f.Class, f.Attempt)
		return harness.Recovery{}
	}
	fmt.Printf("[patient] %s on attempt %d of %d, try again\n", f.Class, f.Attempt, p.tries)
	return harness.Recovery{Retry: true, After: time.Millisecond}
}

func main() {
	fmt.Println("Watch two failed attempts at turn 1, each with a recorded decision, and then a reply.")
	provider, modelName := demo.Provider(fake.Script(fake.Text("Bern.")))
	agent := &harness.Agent{
		Name:         "geo",
		Instructions: "Answer in one word.",
		Provider:     demo.FailFirst(provider, 2, model.ClassTransient),
		Model:        modelName,
		Hooks:        []harness.Hook{patient{tries: 3}},
	}
	demo.Run(demo.Context(), agent, harness.Prompt("What is the capital of Switzerland?"))
}
