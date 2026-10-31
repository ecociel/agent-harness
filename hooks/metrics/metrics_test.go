package metrics_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/metrics"
	"github.com/ecociel/agent-harness/hooks/retry"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
)

// A failed call is counted and its billed tokens with it. A hook never saw these.
func TestFailuresAreCounted(t *testing.T) {
	reg := metrics.NewRegistry()
	n := 0
	p := fake.NewFunc(func(model.Request) (model.Response, error) {
		if n++; n == 1 {
			return model.Response{}, &model.Error{Class: model.ClassTransient, Usage: model.Usage{Input: 7}, Err: errors.New("cut")}
		}
		time.Sleep(2 * time.Millisecond)
		return model.Response{Text: "ok", Usage: model.Usage{Input: 10, Output: 5}}, nil
	})
	a := &harness.Agent{Name: "demo", Model: "m1", Provider: p, Hooks: []harness.Hook{&retry.Retry{Base: 1}}}
	if _, err := a.Run(harness.WithEvents(context.Background(), metrics.Sink(reg)), harness.Prompt("x")); err != nil {
		t.Fatal(err)
	}
	s := reg.Snapshot()
	for key, want := range map[string]float64{
		"model_calls_total{model=m1}":                  2,
		"model_errors_total{class=transient,model=m1}": 1,
		"model_tokens_total{kind=input,model=m1}":      17,
		"model_tokens_total{kind=output,model=m1}":     5,
		"agent_runs_total{agent=demo,stop=completed}":  1,
	} {
		if s.Counters[key] != want {
			t.Errorf("%s = %v, want %v\n%s", key, s.Counters[key], want, s)
		}
	}
	if h := s.Histograms["agent_turns{agent=demo}"]; h.Count != 1 || h.Max != 1 {
		t.Errorf("%+v", h)
	}
	if h := s.Histograms["agent_run_duration_ms{agent=demo}"]; h.Count != 1 || h.Sum < 2 {
		t.Errorf("%+v", h)
	}
}

// A second worker on a finished run writes nothing, so nothing is counted twice.
func TestAFinishedRunIsCountedOnce(t *testing.T) {
	reg := metrics.NewRegistry()
	j := harness.NewMemJournal()
	a := &harness.Agent{Name: "demo", Journal: j, Provider: fake.Script(fake.Text("ok"))}
	in := harness.Prompt("x")
	in.RunID = "r"
	ctx := harness.WithEvents(context.Background(), metrics.Sink(reg))
	for _, i := range []harness.Input{in, harness.Resume("r")} {
		if _, err := a.Run(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	if got := reg.Snapshot().Counters["agent_runs_total{agent=demo,stop=completed}"]; got != 1 {
		t.Fatalf("counted %v times", got)
	}
}

func TestDeniedAndLostCallsAreCounted(t *testing.T) {
	reg := metrics.NewRegistry()
	sink := metrics.Sink(reg)
	sink(harness.Event{Kind: harness.KindToolGated, Tool: "rm", Decision: &harness.Decision{Action: harness.ActionDeny}})
	sink(harness.Event{Kind: harness.KindToolGated, Tool: "ls", Decision: &harness.Decision{}})
	sink(harness.Event{Kind: harness.KindToolLost, Tool: "pay"})
	sink(harness.Event{Kind: harness.KindModelLost, Agent: "demo"})
	s := reg.Snapshot()
	for key, want := range map[string]float64{
		"tool_calls_total{outcome=denied,tool=rm}": 1,
		"tool_calls_total{outcome=denied,tool=ls}": 0,
		"tool_calls_total{outcome=lost,tool=pay}":  1,
		"model_lost_total{agent=demo}":             1,
	} {
		if s.Counters[key] != want {
			t.Errorf("%s = %v, want %v", key, s.Counters[key], want)
		}
	}
}
