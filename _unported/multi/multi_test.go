package multi_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/multi"
	"github.com/ecociel/agent-harness/provider/fake"
)

func broken(name string) *harness.Agent {
	return &harness.Agent{Name: name, Provider: fake.NewFunc(func(model.Request) (model.Response, error) {
		return model.Response{}, errors.New("provider down")
	})}
}

func TestParallelFailsWhenABranchFails(t *testing.T) {
	p := &multi.Parallel{Branches: []harness.Runner{bot("ok", say("fine")), broken("bad")}}
	res, err := p.Run(context.Background(), harness.Prompt("x"))
	if err != nil || res.OK() || !strings.Contains(res.Detail, "bad") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	p.AllowPartial = true
	res, _ = p.Run(context.Background(), harness.Prompt("x"))
	if !res.OK() || !strings.Contains(res.Output, "fine") {
		t.Fatalf("partial: %+v", res)
	}
}

func TestParallelSumsUsage(t *testing.T) {
	p := &multi.Parallel{Branches: []harness.Runner{bot("a", say("1")), bot("b", say("2"))}}
	res, _ := p.Run(context.Background(), harness.Prompt("x"))
	if res.Usage.Total() == 0 {
		t.Fatal("usage not aggregated")
	}
}

func TestBestOfNScoreAndIneligible(t *testing.T) {
	b := &multi.BestOfN{Candidates: []harness.Runner{bot("short", say("ok")), broken("x"), bot("long", say("a longer answer"))},
		Score: func(_ harness.Input, r *harness.Result) float64 { return float64(len(r.Output)) }}
	res, _ := b.Run(context.Background(), harness.Prompt("q"))
	if res.Output != "a longer answer" {
		t.Fatalf("got %q", res.Output)
	}
}

func TestRouterFallbackOnClassifierFailure(t *testing.T) {
	r := &multi.Router{Classifier: broken("cls"), Routes: map[string]harness.Runner{"a": bot("a", say("A"))},
		Fallback: bot("fb", say("FB"))}
	res, _ := r.Run(context.Background(), harness.Prompt("q"))
	if res.Output != "FB" {
		t.Fatalf("got %q", res.Output)
	}
}

func TestSubagentDepthCap(t *testing.T) {
	// An agent that always delegates to itself must be stopped by depth.
	self := &harness.Agent{Name: "self", MaxTurns: 3}
	self.Provider = fake.NewFunc(func(req model.Request) (model.Response, error) {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == model.RoleTool {
			return fake.Text("tool said: " + last.Content), nil
		}
		return fake.Call("again", map[string]string{"task": "recurse"}), nil
	})
	withTool, err := multi.WithSubagents(self, multi.Subagent{Name: "again", Agent: harness.RunnerFunc(
		func(ctx context.Context, in harness.Input) (*harness.Result, error) { return self.Run(ctx, in) }), MaxDepth: 2})
	if err != nil {
		t.Fatal(err)
	}
	self.Tools = withTool.Tools
	res, err := self.Run(context.Background(), harness.Prompt("go"))
	if err != nil || !res.OK() {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Output, "exceeds limit 2") {
		t.Fatalf("depth cap not reported: %q", res.Output)
	}
}

func TestHandoffPingPongIsStopped(t *testing.T) {
	a := &harness.Agent{Name: "a", Provider: fake.NewFunc(func(model.Request) (model.Response, error) {
		return fake.Call("transfer_to_b", nil), nil
	})}
	b := &harness.Agent{Name: "b", Provider: fake.NewFunc(func(model.Request) (model.Response, error) {
		return fake.Call("transfer_to_a", nil), nil
	})}
	h := &multi.Handoff{Agents: []*harness.Agent{a, b}, MaxHops: 3}
	res, _ := h.Run(context.Background(), harness.Prompt("x"))
	if res.Stop != harness.StopLoop {
		t.Fatalf("stop = %s", res.Stop)
	}
}

func TestHandoffSticky(t *testing.T) {
	a := &harness.Agent{Name: "a", Provider: fake.Script(fake.Call("transfer_to_b", nil))}
	b := &harness.Agent{Name: "b", Provider: fake.Script(fake.Text("b1"), fake.Text("b2"))}
	h := &multi.Handoff{Agents: []*harness.Agent{a, b}, Sticky: true}
	s := harness.NewSession(h)
	if _, err := s.Send(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Send(context.Background(), "two")
	if err != nil || res.Output != "b2" || h.Owner() != "b" {
		t.Fatalf("res=%+v err=%v owner=%s", res, err, h.Owner())
	}
}

func TestEvaluatorNoProgressAndCap(t *testing.T) {
	same := &multi.EvaluatorOptimizer{Generator: bot("g", say("same")), Evaluator: bot("e", say("REVISE: more"))}
	res, _ := same.Run(context.Background(), harness.Prompt("x"))
	if res.Stop != harness.StopLoop {
		t.Fatalf("stop = %s", res.Stop)
	}
	n := 0
	var mu sync.Mutex
	changing := &multi.EvaluatorOptimizer{MaxRounds: 2, Evaluator: bot("e", say("REVISE: more")),
		Generator: bot("g", func(string) string { mu.Lock(); defer mu.Unlock(); n++; return strings.Repeat("x", n) })}
	res, _ = changing.Run(context.Background(), harness.Prompt("x"))
	if res.Stop != harness.StopMaxRepairs || res.Output != "xx" {
		t.Fatalf("stop=%s out=%q", res.Stop, res.Output)
	}
}

func TestPlannerWorkerSubPlanner(t *testing.T) {
	planner := bot("planner", func(p string) string {
		if strings.HasPrefix(p, "Goal:\nfrontend") {
			return "TASK: build the form\nDONE"
		}
		return "PLAN: frontend\nTASK: write the API\nDONE"
	})
	worker := bot("worker", func(p string) string {
		i := strings.Index(p, "Your task:\n")
		return "did " + strings.SplitN(p[i+len("Your task:\n"):], "\n", 2)[0]
	})
	pw := &multi.PlannerWorker{Planner: planner, Worker: worker}
	res, _ := pw.Run(context.Background(), harness.Prompt("app"))
	for _, want := range []string{"did build the form", "did write the API"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("missing %q in %q", want, res.Output)
		}
	}
}

func TestPatternEventsAndNesting(t *testing.T) {
	var mu sync.Mutex
	var evs []harness.Event
	ctx := harness.WithEvents(context.Background(), func(e harness.Event) { mu.Lock(); evs = append(evs, e); mu.Unlock() })
	p := &multi.Parallel{Name: "fan", Branches: []harness.Runner{bot("a", say("1")), bot("b", say("2"))}}
	if _, err := p.Run(ctx, harness.Prompt("x")); err != nil {
		t.Fatal(err)
	}
	var patternID string
	starts := 0
	for _, e := range evs {
		if e.Kind == harness.KindPattern && e.Outcome == "start" {
			patternID = e.RunID
		}
		if e.Kind == harness.KindRunStart {
			starts++
			if e.ParentRun == "" || e.Depth != 1 {
				t.Fatalf("branch run not nested: %+v", e)
			}
		}
	}
	if patternID == "" || starts != 2 {
		t.Fatalf("pattern=%q starts=%d", patternID, starts)
	}
}
