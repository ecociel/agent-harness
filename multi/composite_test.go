package multi_test

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/approval"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/multi"
	"github.com/ecociel/agent-harness/tool"
)

// A pattern records itself and nothing else: no model call nobody paid for, no tool
// call nobody made. So a plain sum over the tree is the bill.
func TestNoPatternWritesAModelEvent(t *testing.T) {
	type tree func(w *world, j harness.Journal) harness.Runner
	cases := map[string]tree{
		"parallel": func(w *world, j harness.Journal) harness.Runner {
			return &multi.Parallel{Name: "fan", Journal: j, Branches: []harness.Runner{w.agent("a", "one"), w.agent("b", "two")}}
		},
		"aggregator": func(w *world, j harness.Journal) harness.Runner {
			return &multi.Parallel{Name: "fan", Journal: j, Aggregator: w.agent("c", "three"),
				Branches: []harness.Runner{w.agent("a", "one"), w.agent("b", "two")}}
		},
		"sequential": func(w *world, j harness.Journal) harness.Runner {
			return &multi.Sequential{Name: "pipe", Journal: j, Steps: []harness.Runner{w.agent("a", "one"), w.agent("b", "two")}}
		},
		"nested with a wait": func(w *world, j harness.Journal) harness.Runner {
			return &multi.Parallel{Name: "fan", Journal: j, Branches: []harness.Runner{
				w.agent("a", "one"),
				&multi.Sequential{Name: "pipe", Steps: []harness.Runner{w.agent("b", "two", asks()), w.agent("c", "three")}},
			}}
		},
		"below a lead": func(w *world, j harness.Journal) harness.Runner {
			return lead(w, j, &multi.Sequential{Name: "pipe", Steps: []harness.Runner{w.agent("a", "one")}})
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			j := newJournal()
			w := newWorld()
			res := drive(t, w, func() harness.Runner { return build(w, j) }, start(t, build(w, j)))
			if !res.OK() {
				t.Fatalf("%+v", res)
			}
			composites := 0
			for _, id := range j.ids() {
				r := fold(t, j, id)
				if r.Pin.Shape == "" {
					continue
				}
				composites++
				for _, e := range r.Events {
					if strings.HasPrefix(string(e.Kind), "model.") || strings.HasPrefix(string(e.Kind), "tool.") {
						t.Errorf("composite %s wrote %s", id, e.Kind)
					}
				}
				if r.ModelCalls+r.LostCalls+r.ToolCalls != 0 || r.Usage.Total() != 0 {
					t.Errorf("composite %s holds spend of its own: %+v", id, r)
				}
			}
			if composites == 0 {
				t.Fatal("no composite in the tree, so this proves nothing")
			}
			if calls, _, input := spend(t, j); calls != w.billed() || input != 10*w.billed() {
				t.Fatalf("a plain sum says %d calls and %d tokens, the provider billed %d calls", calls, input, w.billed())
			}
		})
	}
}

// start begins a run under the fixed ID and hands back that ID for drive.
func start(t *testing.T, r harness.Runner) string {
	t.Helper()
	in := harness.Prompt("the task")
	in.RunID = "root"
	if _, err := r.Run(context.Background(), in); err != nil {
		t.Fatalf("start: %v", err)
	}
	return in.RunID
}

// What Run returned and what any process folds from the journal are the same run, so
// nothing about a pattern lives outside apply.
func TestAPatternsRunIsItsFold(t *testing.T) {
	j := newJournal()
	w := newWorld()
	p := &multi.Sequential{Name: "pipe", Journal: j, Steps: []harness.Runner{w.agent("a", "one"), w.agent("b", "two")}}
	in := harness.Prompt("the task")
	in.RunID = "root"
	res, err := p.Run(context.Background(), in)
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	r := fold(t, j, "root")
	if r.Output != res.Output || r.Stop != res.Stop || r.State != res.State || r.Spent() != res.Spent() {
		t.Fatalf("returned %+v\nfolded %+v", res, r)
	}
	if len(r.Messages) != 2 || r.Messages[0].Content != "the task" || r.Messages[1].Content != res.Output {
		t.Fatalf("the transcript should be the input and the answer: %+v", r.Messages)
	}
}

func TestANestedPatternReportsItsWholeTree(t *testing.T) {
	j := newJournal()
	w := newWorld()
	p := &multi.Parallel{Journal: j, Branches: []harness.Runner{
		w.agent("a", "one"),
		&multi.Sequential{Steps: []harness.Runner{w.agent("b", "two"), w.agent("c", "three")}},
	}}
	in := harness.Prompt("the task")
	in.RunID = "root"
	res, err := p.Run(context.Background(), in)
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	calls, _, input := spend(t, j)
	got := res.Spent()
	if got.ModelCalls != calls || got.Usage.Input != input || got.ToolCalls != 3 || got.Turns != 6 {
		t.Fatalf("the pattern reports %+v, its tree holds %d calls and %d tokens", got, calls, input)
	}
}

// A child that stopped hands up what it had, so the caller is not left with less than
// the child itself returned.
func TestAPatternStoppedByAChildKeepsItsOutput(t *testing.T) {
	cut := bot("cut", "half an answer")
	cut.Hooks = []harness.Hook{truncate{}}
	p := &multi.Sequential{Steps: []harness.Runner{cut, bot("never", "x")}}
	res, err := p.Run(context.Background(), harness.Prompt("x"))
	if err != nil || res.Stop != harness.StopMaxRepairs || res.State != harness.StateStopped {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Output != "half an answer" || !strings.Contains(res.Detail, "cut stopped") {
		t.Fatalf("%+v", res)
	}
	if n := len(res.Messages); n != 2 || res.Messages[n-1].Content != "half an answer" {
		t.Fatalf("%+v", res.Messages)
	}
}

type truncate struct{}

func (truncate) Name() string { return "truncate" }
func (truncate) CheckOutput(context.Context, harness.Run, string) harness.Verdict {
	return harness.Revise("again")
}

type verdict struct{ v harness.Verdict }

func (verdict) Name() string { return "verdict" }
func (o verdict) CheckOutput(context.Context, harness.Run, string) harness.Verdict {
	return o.v
}

// CheckOutput on a pattern is a guardrail like any other. Dropping it because a
// pattern has no model would bypass it silently.
func TestCheckOutputStillJudgesAPattern(t *testing.T) {
	run := func(v harness.Verdict) (*harness.Run, *jsonJournal) {
		j := newJournal()
		p := &multi.Sequential{Journal: j, Hooks: []harness.Hook{verdict{v}}, Steps: []harness.Runner{bot("a", "answer")}}
		in := harness.Prompt("x")
		in.RunID = "root"
		res, err := p.Run(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		return res, j
	}
	if res, _ := run(harness.Block("no")); res.Stop != harness.StopGuardrail || res.Output != "" {
		t.Fatalf("a blocked answer must not be the output: %+v", res)
	}
	res, _ := run(harness.Accept())
	if !res.OK() || res.Output != "answer" {
		t.Fatalf("%+v", res)
	}
	res, _ = run(harness.Verdict{Rewrite: "better"})
	if !res.OK() || res.Output != "better" || res.Messages[len(res.Messages)-1].Content != "better" {
		t.Fatalf("%+v", res)
	}
	// No model is here to revise, so it ends at once rather than walking the tree again.
	res, j := run(harness.Revise("again"))
	if res.Stop != harness.StopMaxRepairs || res.Output != "answer" {
		t.Fatalf("%+v", res)
	}
	if n := fold(t, j, "root").Claims; n != 0 {
		t.Fatalf("a revise claimed the run %d times", n)
	}
}

// A policy with no threshold used to gate the scripted body call. A pattern has no
// calls to gate now, so only real tools meet it.
func TestAToolPolicyOnAPatternHasNothingToGate(t *testing.T) {
	policy := &approval.Policy{Threshold: tool.RiskRead, Ask: true}
	p := &multi.Sequential{Journal: newJournal(), Hooks: []harness.Hook{policy}, Steps: []harness.Runner{bot("a", "answer")}}
	in := harness.Prompt("x")
	in.RunID = "root"
	res, err := p.Run(context.Background(), in)
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
}

// The pattern's input is in its journal, so a resume needs nothing but the ID. What a
// caller passes later cannot change what the run was asked.
func TestAResumeUsesTheRecordedInput(t *testing.T) {
	ctx := context.Background()
	j := newJournal()
	w := newWorld()
	build := func(jj harness.Journal) *multi.Parallel {
		return &multi.Parallel{Journal: jj, Branches: []harness.Runner{echo("a"), w.agent("b", "pay")}}
	}
	in := harness.Prompt("the first request")
	in.RunID = "root"
	if _, err := build(kill(j, "root/1", 3, false)).Run(ctx, in); !errors.Is(err, errDead) {
		t.Fatal(err)
	}
	other := harness.Prompt("a different request")
	other.RunID = "root"
	res, err := build(j).Run(ctx, other)
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if !strings.Contains(res.Output, "the first request") || strings.Contains(res.Output, "different") {
		t.Fatalf("%q", res.Output)
	}
}

func echo(name string) *harness.Agent {
	return &harness.Agent{Name: name, Provider: model.ProviderFunc(func(_ context.Context, req model.Request) iter.Seq2[model.Event, error] {
		return model.Stream(model.Response{Text: "heard " + model.LastUser(req.Messages)})
	})}
}
