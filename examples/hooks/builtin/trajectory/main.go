// Command trajectory shows hooks/trajectory. A trajectory is the whole run in one value
// for evals, replay and debugging: messages, tool outcomes, state path and usage. The
// Recorder keeps one per finished run. trajectory.From reads the same thing from any
// Run, so it also works on a run loaded from a journal long after it ended.
//
//	go run ./examples/hooks/builtin/trajectory
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/builtin/trajectory
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/hooks/trajectory"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func main() {
	fmt.Println("Watch the run, then the same run read back as a trajectory.")
	provider, modelName := demo.Provider(fake.Script(
		fake.Call("weather", map[string]string{"city": "Zurich"}),
		fake.Call("weather", map[string]string{"city": "Bern"}),
		fake.Text("Both are at 11 °C."),
	))
	weather := tool.Func(tool.Spec{
		Name:        "weather",
		Description: "Current weather for a city.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		Effect:      tool.EffectRead,
	}, func(context.Context, json.RawMessage) (string, error) { return `{"temp_c":11}`, nil })
	recorder := trajectory.New()
	agent := &harness.Agent{
		Name:         "weather",
		Instructions: "Answer briefly. Use the weather tool once per city.",
		Provider:     provider,
		Model:        modelName,
		Tools:        tool.NewRegistry(weather),
		Hooks:        []harness.Hook{recorder},
	}
	res := demo.Run(demo.Context(), agent, harness.Prompt("Compare the weather in Zurich and Bern."))
	t, ok := recorder.Get(res.ID)
	if !ok {
		panic("no trajectory for " + res.ID)
	}
	fmt.Printf("\ntrajectory of %s: %d turns, %d messages, stop %s\n", t.RunID, t.Turns, len(t.Messages), t.Stop)
	for _, s := range t.Steps {
		fmt.Printf("turn %d: %s %s -> %s (%s)\n", s.Turn, s.Tool, s.Args, s.Result, s.Outcome)
	}
}
