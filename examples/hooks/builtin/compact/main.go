// Command compact shows hooks/compact. When the estimated history exceeds MaxTokens, the
// oldest messages are replaced by one summary and the last KeepLast stay as they are.
// The edit is recorded, so a summary is computed once and survives a crash. Digest is
// the model-free summariser. compact.ModelSummarizer asks a model instead.
//
//	go run ./examples/hooks/builtin/compact
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/builtin/compact
package main

import (
	"fmt"
	"strings"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/hooks/compact"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
)

func main() {
	fmt.Println("Watch the history edit before the only model call: five messages become one summary.")
	provider, modelName := demo.Provider(fake.Script(fake.Text("Order 4711 ships on Tuesday.")))
	agent := &harness.Agent{
		Name:         "support",
		Instructions: "Answer in one sentence.",
		Provider:     provider,
		Model:        modelName,
		Hooks:        []harness.Hook{&compact.Compactor{MaxTokens: 20, KeepLast: 2, Summarize: compact.Digest}},
	}
	history := []model.Message{
		model.User("Hi, I have a question about my order."),
		model.Assistant("Of course. What is the order number?"),
		model.User("It is order 4711, the blue kettle."),
		model.Assistant("Thanks, I found it. It left our warehouse this morning."),
		model.User("Can it still go to my office instead?"),
		model.Assistant("Yes, I changed the address to your office."),
		model.User("When does order 4711 ship?"),
	}
	res := demo.Run(demo.Context(), agent, harness.Input{Messages: history})
	fmt.Printf("history went from %d messages to %d\n", len(history), len(res.Messages))
	for _, line := range strings.Split(strings.TrimSpace(res.Messages[0].Content), "\n") {
		fmt.Printf("| %s\n", line)
	}
}
