// Command metrics shows hooks/metrics. Metrics are a sink, not a hook: a hook can repeat
// after a crash and count twice, and it never sees a call that failed. Attach the sink
// with harness.WithEvents and read a Snapshot, or implement Recorder over your own
// client. Only the counters are printed here, because the histograms hold real latencies
// that differ on every run.
//
//	go run ./examples/hooks/builtin/metrics
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/builtin/metrics
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/hooks/metrics"
	"github.com/ecociel/agent-harness/hooks/retry"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func main() {
	fmt.Println("Watch the run, then the counters: the failed attempt is counted too.")
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
		Provider:     demo.FailFirst(provider, 1, model.ClassTransient),
		Model:        modelName,
		Tools:        tool.NewRegistry(weather),
		Hooks:        []harness.Hook{&retry.Retry{Base: time.Millisecond}},
	}
	registry := metrics.NewRegistry()
	ctx := harness.WithEvents(demo.Context(), metrics.Sink(registry))
	demo.Run(ctx, agent, harness.Prompt("What is the weather in Zurich?"))

	fmt.Println()
	counters := registry.Snapshot().Counters
	for _, name := range slices.Sorted(maps.Keys(counters)) {
		fmt.Printf("%s %v\n", name, counters[name])
	}
}
