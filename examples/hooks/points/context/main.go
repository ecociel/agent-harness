// Command context shows the Context point. It runs before every model call, and the
// Edit it returns is recorded, so this is the only durable way to change history.
// Here it loads a memory once, with Source set, and leaves later turns alone: the
// history already holds it, so the hook needs no state of its own.
//
//	go run ./examples/hooks/points/context
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/points/context
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

type memory struct{}

func (memory) Name() string { return "memory" }

func (memory) Context(_ context.Context, r harness.Run) (harness.Edit, error) {
	loaded := slices.ContainsFunc(r.Messages, func(m model.Message) bool { return m.Source == "memory" })
	if loaded {
		fmt.Println("[memory] already in the history, nothing to do")
		return harness.Edit{}, nil
	}
	fmt.Println("[memory] not in the history yet, insert it at the top")
	return harness.Edit{With: []model.Message{model.User("From memory: I live in Zurich.")}, Source: "memory"}, nil
}

func main() {
	fmt.Println("Watch the first turn edit the history and the second leave it alone.")
	provider, modelName := demo.Provider(fake.Script(
		fake.Call("weather", map[string]string{"city": "Zurich"}),
		fake.Text("It is 11 °C in Zurich."),
	))
	weather := tool.Func(tool.Spec{
		Name:        "weather",
		Description: "Current weather for a city.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		Effect:      tool.EffectRead,
	}, func(context.Context, json.RawMessage) (string, error) { return `{"temp_c":11}`, nil })
	agent := &harness.Agent{
		Name:         "weather",
		Instructions: "Answer briefly. Use the weather tool.",
		Provider:     provider,
		Model:        modelName,
		Tools:        tool.NewRegistry(weather),
		Hooks:        []harness.Hook{memory{}},
	}
	res := demo.Run(demo.Context(), agent, harness.Prompt("What is the weather where I live?"))
	fmt.Printf("the history starts with %q, source %q\n", res.Messages[0].Content, res.Messages[0].Source)
}
