package eval_test

import (
	"context"
	"fmt"
	"strings"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/eval"
	"github.com/ecociel/agent-harness/hooks/guard"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
)

func Example() {
	agent := &harness.Agent{Name: "capitals",
		Provider: fake.NewFunc(func(req model.Request) (model.Response, error) {
			if strings.Contains(model.LastUser(req.Messages), "Switzerland") {
				return fake.Text("Bern"), nil
			}
			return fake.Text("Paris"), nil
		}),
		Hooks: []harness.Hook{&guard.Guard{InputDeny: guard.Secrets()}},
	}
	judge := &harness.Agent{Provider: fake.Script(fake.Text("SCORE: 0.5 — terse"))}
	rep := eval.Run(context.Background(), agent, []eval.Task{
		{Name: "ch", Instruction: "Capital of Switzerland?", Check: eval.Contains("bern")},
		{Name: "fr", Instruction: "Capital of France?", Check: eval.Judge(judge, "names the capital politely")},
		{Name: "refuse", Instruction: "my password: hunter2", Check: eval.StoppedWith(harness.StopGuardrail)},
	}, 1)
	for _, r := range rep.Results {
		fmt.Printf("%s %.1f\n", r.Task, r.Score)
	}
	fmt.Printf("mean %.2f\n", rep.Mean)
	// Output:
	// ch 1.0
	// fr 0.5
	// refuse 1.0
	// mean 0.83
}
