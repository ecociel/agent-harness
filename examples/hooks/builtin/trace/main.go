// Command trace shows hooks/trace. Tracing is a sink, not a hook: attach it once with
// harness.WithEvents and every run in the tree reports to it. Agent.Hooks stays empty.
// A span's ID is a hash of the run and the step, so it survives a resume in another
// process. With a fixed RunID these IDs are the same every time.
//
//	go run ./examples/hooks/builtin/trace
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/builtin/trace
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/hooks/trace"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func main() {
	fmt.Println("Watch the run, then its spans: one per run, model call and tool call.")
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
	}
	spans := &trace.Memory{}
	ctx := harness.WithEvents(demo.Context(), trace.Sink(spans))
	demo.Run(ctx, agent, harness.Prompt("What is the weather in Zurich?"))

	fmt.Println()
	names := map[string]string{}
	for _, s := range spans.Spans() {
		names[s.SpanID] = s.Name
	}
	for _, s := range spans.Spans() {
		parent := names[s.ParentID]
		if parent == "" {
			parent = "none, it is the root"
		}
		fmt.Printf("span %s %q status %s, parent: %s, operation %v\n", s.SpanID, s.Name, s.Status, parent, s.Attrs["gen_ai.operation.name"])
	}
}
