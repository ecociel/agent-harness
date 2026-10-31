// Command wrapmodel shows the WrapModel point. A hook wraps the provider stream, so it
// sees each chunk before the collector and the sinks. It may rewrite chunks or end the
// stream with a Stop error, which halts the run mid-reply. An attempt made in here is
// not journaled, so retries belong in Recover.
//
//	go run ./examples/hooks/points/wrapmodel
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/points/wrapmodel
package main

import (
	"context"
	"fmt"
	"iter"
	"strings"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
)

// watchword ends the stream at the first chunk holding the word.
type watchword struct{ word string }

func (watchword) Name() string { return "watchword" }

func (w watchword) WrapModel(_ harness.Run, next model.Provider) model.Provider {
	return model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
		return func(yield func(model.Event, error) bool) {
			for ev, err := range next.Complete(ctx, req) {
				if d, ok := ev.(model.TextDelta); ok {
					if strings.Contains(d.Text, w.word) {
						fmt.Println("[watchword] this chunk holds the watchword, cut the stream here")
						yield(nil, harness.Stop(harness.StopGuardrail, "the reply leaks the password"))
						return
					}
					fmt.Printf("[watchword] chunk %q\n", d.Text)
				}
				if !yield(ev, err) {
					return
				}
			}
		}
	})
}

func main() {
	fmt.Println("Watch the chunks: the first reply passes, the second is cut mid-sentence.")
	provider, modelName := demo.Provider(fake.Script(
		fake.Text("The office opens at 8."),
		fake.Text("The staff wifi password is tulip, ask at reception."),
	))
	agent := &harness.Agent{
		Name:         "reception",
		Instructions: "Answer in one sentence. The office opens at 8. The staff wifi password is tulip.",
		Provider:     provider,
		Model:        modelName,
		Hooks:        []harness.Hook{watchword{word: "tulip"}},
	}
	ctx := demo.Context()
	demo.Run(ctx, agent, harness.Prompt("When does the office open?"))
	demo.Run(ctx, agent, harness.Prompt("What is the staff wifi password?"))
}
