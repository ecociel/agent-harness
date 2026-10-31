package eval_test

import (
	"context"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/eval"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/multi"
	"github.com/ecociel/agent-harness/provider/fake"
)

// A pattern spends nothing itself, so a report that read only its own counters would
// call every multi-agent run free.
func TestAReportCountsWhatAPatternsChildrenSpent(t *testing.T) {
	step := func(name string) *harness.Agent {
		return &harness.Agent{Name: name, Provider: fake.NewFunc(func(model.Request) (model.Response, error) {
			return model.Response{Text: "done", Usage: model.Usage{Input: 10, Output: 1}}, nil
		})}
	}
	p := &multi.Sequential{Steps: []harness.Runner{step("a"), step("b")}}
	rep := eval.Run(context.Background(), p, []eval.Task{{Name: "t", Instruction: "x", Check: eval.Contains("done")}}, 1)
	if rep.Mean != 1 || rep.Usage.Input != 20 {
		t.Fatalf("%+v", rep)
	}
}
