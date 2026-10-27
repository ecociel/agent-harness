package harness_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/budget"
	"github.com/ecociel/agent-harness/hooks/guard"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func ExampleAgent_Run() {
	weather := tool.Func(tool.Spec{Name: "weather", Description: "Weather for a city."},
		func(_ context.Context, args json.RawMessage) (string, error) {
			return `{"city":"Zurich","temp_c":11}`, nil
		})
	a := &harness.Agent{
		Name:         "assistant",
		Instructions: "Answer briefly.",
		Provider: fake.Script(
			fake.Call("weather", map[string]string{"city": "Zurich"}),
			fake.Text("It is 11 °C in Zurich."),
		),
		Tools: tool.NewRegistry(weather),
		Hooks: []harness.Hook{
			&guard.Guard{InputDeny: guard.Secrets()},
			budget.New(budget.Limits{Tokens: 10_000}),
		},
	}
	res, err := a.Run(context.Background(), harness.Prompt("Weather in Zurich?"))
	if err != nil {
		panic(err)
	}
	fmt.Println(res.Output)
	fmt.Println(res.Stop, res.Path)
	// Output:
	// It is 11 °C in Zurich.
	// completed [start prepare compose model route tools observe compose model route verify done]
}

func ExampleWithEvents() {
	ctx := harness.WithEvents(context.Background(), func(e harness.Event) {
		if e.Kind == harness.KindTransition {
			fmt.Printf("%s → %s\n", e.From, e.To)
		}
	})
	a := &harness.Agent{Provider: fake.Script(fake.Text("hi"))}
	_, _ = a.Run(ctx, harness.Prompt("hello"))
	// Output:
	// start → prepare
	// prepare → compose
	// compose → model
	// model → route
	// route → verify
	// verify → done
}
