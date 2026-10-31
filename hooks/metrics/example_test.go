package metrics_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/metrics"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func Example() {
	reg := metrics.NewRegistry()
	clock := tool.Func(tool.Spec{Name: "clock"}, func(context.Context, json.RawMessage) (string, error) {
		return "12:00", nil
	})
	a := &harness.Agent{
		Name:     "demo",
		Model:    "fake-1",
		Provider: fake.Script(fake.Call("clock", nil), fake.Text("It is noon.")),
		Tools:    tool.NewRegistry(clock),
	}
	ctx := harness.WithEvents(context.Background(), metrics.Sink(reg))
	if _, err := a.Run(ctx, harness.Prompt("time?")); err != nil {
		panic(err)
	}
	s := reg.Snapshot()
	fmt.Println(s.Counters["agent_runs_total{agent=demo,stop=completed}"])
	fmt.Println(s.Counters["model_calls_total{model=fake-1}"])
	fmt.Println(s.Counters["tool_calls_total{outcome=ok,tool=clock}"])
	// Output:
	// 1
	// 2
	// 1
}
