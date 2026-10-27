package budget_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/budget"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func used(in, out int) model.Usage { return model.Usage{Input: in, Output: out} }

func TestTokenLimitStopsSecondTurn(t *testing.T) {
	b := budget.New(budget.Limits{Tokens: 100})
	p := fake.Script(
		model.Response{ToolCalls: []model.ToolCall{{ID: "1", Name: "noop", Arguments: json.RawMessage(`{}`)}}, Usage: used(80, 30)},
		fake.Text("never"))
	noop := tool.Func(tool.Spec{Name: "noop"}, func(context.Context, json.RawMessage) (string, error) { return "", nil })
	a := &harness.Agent{Provider: p, Tools: tool.NewRegistry(noop), Hooks: []harness.Hook{b}}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != harness.StopBudget {
		t.Fatalf("stop = %s (%s)", res.Stop, res.Detail)
	}
	if b.Spent().Usage.Total() != 110 {
		t.Fatalf("spent = %+v", b.Spent())
	}
}

func TestRepeatDetection(t *testing.T) {
	call := model.Response{ToolCalls: []model.ToolCall{{ID: "1", Name: "noop", Arguments: json.RawMessage(`{"q":1}`)}}}
	p := fake.Script(call, call, call, fake.Text("x"))
	noop := tool.Func(tool.Spec{Name: "noop"}, func(context.Context, json.RawMessage) (string, error) { return "", nil })
	a := &harness.Agent{Provider: p, Tools: tool.NewRegistry(noop),
		Hooks: []harness.Hook{budget.New(budget.Limits{RepeatCalls: 3})}}
	res, _ := a.Run(context.Background(), harness.Prompt("x"))
	if res.Stop != harness.StopLoop || res.Turns != 3 {
		t.Fatalf("stop=%s turns=%d", res.Stop, res.Turns)
	}
}

func TestChildIsCarvedAndDebitsParent(t *testing.T) {
	parent := budget.New(budget.Limits{Tokens: 1000}).WithPrice(budget.Price{InputPerM: 1e6})
	parent.Charge(used(600, 0))
	child := parent.Child(budget.Limits{Tokens: 5000})
	if got := child.Limits().Tokens; got != 400 {
		t.Fatalf("child tokens = %d, want 400 (parent remaining)", got)
	}
	child.Charge(used(100, 0))
	if got := parent.Spent().Usage.Total(); got != 700 {
		t.Fatalf("parent spent = %d", got)
	}
	if got := parent.Spent().CostUSD; got != 700 {
		t.Fatalf("cost = %v", got)
	}
	parent.Charge(used(300, 0))
	if _, ok := child.Exceeded(); !ok {
		t.Fatal("child should see the exhausted parent")
	}
}

func TestPreflightEstimate(t *testing.T) {
	b := budget.New(budget.Limits{Tokens: 5})
	a := &harness.Agent{Provider: fake.Script(fake.Text("x")), Hooks: []harness.Hook{b}}
	res, _ := a.Run(context.Background(), harness.Prompt("a long prompt that is clearly more than five tokens"))
	if res.Stop != harness.StopBudget || res.ModelCalls != 0 {
		t.Fatalf("stop=%s calls=%d", res.Stop, res.ModelCalls)
	}
}
