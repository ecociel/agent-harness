package multi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/multi"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

// answers makes an agent from a pure function of its request, as a sweep needs: reply
// gives the text, or a tool to call first. Each call bills like a model's.
func (w *world) answering(name string, reply func(prompt string, used []string) (text, call string), hooks []harness.Hook, tools ...string) *harness.Agent {
	var ts []tool.Tool
	for _, t := range tools {
		ts = append(ts, w.tool(t, tool.Spec{Effect: tool.EffectOnce, Risk: tool.RiskDangerous}))
	}
	return &harness.Agent{Name: name, Hooks: hooks, Tools: tool.NewRegistry(ts...),
		Provider: model.ProviderFunc(func(_ context.Context, req model.Request) iter.Seq2[model.Event, error] {
			w.mu.Lock()
			w.paid++
			w.mu.Unlock()
			var used []string
			for _, m := range req.Messages {
				if m.Role == model.RoleUser {
					used = nil
				}
				if m.Role == model.RoleTool {
					used = append(used, m.Name)
				}
			}
			text, call := reply(model.LastUser(req.Messages), used)
			if call != "" {
				return model.Stream(model.Response{Usage: model.Usage{Input: 10, Output: 1},
					ToolCalls: []model.ToolCall{{ID: "c" + call, Name: call, Arguments: json.RawMessage(`{"task":"go"}`)}}})
			}
			return model.Stream(model.Response{Text: text, Usage: model.Usage{Input: 10, Output: 1}})
		})}
}

// paying calls its tool once, then answers.
func (w *world) paying(name, toolName string, hooks ...harness.Hook) *harness.Agent {
	return w.answering(name, func(prompt string, used []string) (string, string) {
		if len(used) == 0 {
			return "", toolName
		}
		return name + " paid for " + prompt, ""
	}, hooks, toolName)
}

func (w *world) saying(name, text string) *harness.Agent {
	return w.answering(name, func(string, []string) (string, string) { return text, "" }, nil)
}

func TestCrashSweepThroughBestOfN(t *testing.T) {
	sweep(t, scenario{id: "root", tools: []string{"pay_a", "pay_b"}, reportsTree: true,
		build: func(w *world, j harness.Journal) harness.Runner {
			return &multi.BestOfN{Name: "best", Journal: j, Judge: w.saying("judge", "WINNER: 2"),
				Candidates: []harness.Runner{w.paying("a", "pay_a"), w.paying("b", "pay_b", asks())}}
		}})
}

func TestCrashSweepThroughDebate(t *testing.T) {
	debater := func(w *world, name, pay string, hooks ...harness.Hook) *harness.Agent {
		return w.answering(name, func(prompt string, used []string) (string, string) {
			if !strings.Contains(prompt, "Reconsider") && len(used) == 0 {
				return "", pay
			}
			return name + " on " + fmt.Sprint(len(prompt)), ""
		}, hooks, pay)
	}
	sweep(t, scenario{id: "root", tools: []string{"pay_a", "pay_b"}, reportsTree: true,
		build: func(w *world, j harness.Journal) harness.Runner {
			return &multi.Debate{Name: "debate", Journal: j, Judge: w.saying("judge", "WINNER: 1"),
				Debaters: []harness.Runner{debater(w, "a", "pay_a"), debater(w, "b", "pay_b", asks())}}
		}})
}

func TestCrashSweepThroughEvaluatorOptimizer(t *testing.T) {
	sweep(t, scenario{id: "root", tools: []string{"draft", "revise"}, reportsTree: true,
		build: func(w *world, j harness.Journal) harness.Runner {
			gen := w.answering("gen", func(prompt string, used []string) (string, string) {
				switch revising := strings.Contains(prompt, "Reviewer feedback"); {
				case len(used) == 0 && revising:
					return "", "revise"
				case len(used) == 0:
					return "", "draft"
				case revising:
					return "second draft", ""
				}
				return "first draft", ""
			}, []harness.Hook{asks()}, "draft", "revise")
			eval := w.answering("eval", func(prompt string, _ []string) (string, string) {
				if strings.Contains(prompt, "second draft") {
					return "APPROVED", ""
				}
				return "REVISE: more", ""
			}, nil)
			return &multi.EvaluatorOptimizer{Name: "eo", Journal: j, Generator: gen, Evaluator: eval}
		}})
}

func TestCrashSweepThroughRouter(t *testing.T) {
	sweep(t, scenario{id: "root", tools: []string{"pay"}, reportsTree: true,
		build: func(w *world, j harness.Journal) harness.Runner {
			return &multi.Router{Name: "route", Journal: j, Classifier: w.saying("classifier", "billing"),
				Routes:   map[string]harness.Runner{"billing": w.paying("billing", "pay", asks()), "other": w.saying("other", "no")},
				Fallback: w.saying("fallback", "no")}
		}})
}

func TestCrashSweepThroughPlannerWorker(t *testing.T) {
	sweep(t, scenario{id: "root", tools: []string{"one", "two", "deep"}, reportsTree: true,
		build: func(w *world, j harness.Journal) harness.Runner {
			planner := w.answering("planner", func(prompt string, _ []string) (string, string) {
				switch {
				case strings.Contains(prompt, "Report 1"):
					return "DONE", ""
				case strings.HasPrefix(prompt, "Goal:\nthe slice"):
					return "TASK: deep", ""
				}
				return "PLAN: the slice\nTASK: one\nTASK: two", ""
			}, nil)
			worker := w.answering("worker", func(prompt string, used []string) (string, string) {
				task := strings.Split(strings.SplitN(prompt, "Your task:\n", 2)[1], "\n")[0]
				if len(used) == 0 {
					return "", task
				}
				return "did " + task, ""
			}, []harness.Hook{asks()}, "one", "two", "deep")
			return &multi.PlannerWorker{Name: "pw", Journal: j, Planner: planner, Worker: worker,
				Judge: w.saying("judge", "CONTINUE"), Synthesizer: w.answering("synth", func(prompt string, _ []string) (string, string) {
					return fmt.Sprint("summary of ", strings.Count(prompt, "did "), " reports"), ""
				}, nil)}
		}})
}

func TestCrashSweepThroughHandoff(t *testing.T) {
	sweep(t, scenario{id: "root", tools: []string{"pay_a", "pay_b"}, reportsTree: true,
		build: func(w *world, j harness.Journal) harness.Runner {
			a := w.answering("a", func(_ string, used []string) (string, string) {
				if len(used) == 0 {
					return "", "pay_a"
				}
				return "", "transfer_to_b"
			}, nil, "pay_a")
			b := w.answering("b", func(_ string, used []string) (string, string) {
				if len(used) == 0 {
					return "", "pay_b"
				}
				return "b finished", ""
			}, []harness.Hook{asks()}, "pay_b")
			return &multi.Handoff{Name: "desk", Journal: j, Agents: []*harness.Agent{a, b}}
		}})
}

func TestCrashSweepThroughTeam(t *testing.T) {
	sweep(t, scenario{id: "root", tools: []string{"one", "two", "three"}, reportsTree: true,
		build: func(w *world, j harness.Journal) harness.Runner {
			member := func(name string, hooks ...harness.Hook) *harness.Agent {
				return w.answering(name, func(prompt string, used []string) (string, string) {
					task := strings.Split(strings.SplitN(prompt, "):\n", 2)[1], "\n")[0]
					switch {
					case len(used) == 0:
						return "", task
					case task == "one":
						return "did one\nNEW TASK: three", ""
					}
					return "did " + task, ""
				}, hooks, "one", "two", "three")
			}
			return &multi.Team{Name: "team", Journal: j, Members: []harness.Runner{member("m1"), member("m2", asks())},
				Seed: func(harness.Input) []string { return []string{"one", "two"} }}
		}})
}

func TestNoNewPatternWritesAModelEvent(t *testing.T) {
	w := newWorld()
	j := newJournal()
	in := harness.Prompt("x")
	in.RunID = "root"
	r := &multi.BestOfN{Journal: j, Candidates: []harness.Runner{w.saying("a", "1"), w.saying("b", "22")},
		Score: func(_ harness.Input, r *harness.Run) float64 { return float64(len(r.Output)) }}
	if res, err := r.Run(context.Background(), in); err != nil || res.Output != "22" {
		t.Fatalf("%v %+v", err, res)
	}
	if strings.Contains(fold(t, j, "root").Pin.Shape, "model") {
		t.Fatal("shape")
	}
	for _, e := range fold(t, j, "root").Events {
		if strings.HasPrefix(string(e.Kind), "model.") {
			t.Fatalf("a pattern wrote %s", e.Kind)
		}
	}
}

func TestBestOfNScoreAndIneligible(t *testing.T) {
	b := &multi.BestOfN{Candidates: []harness.Runner{bot("short", "ok"), broken("x"), bot("long", "a longer answer")},
		Score: func(_ harness.Input, r *harness.Run) float64 { return float64(len(r.Output)) }}
	res, err := b.Run(context.Background(), harness.Prompt("q"))
	if err != nil || res.Output != "a longer answer" {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestBestOfNWithNothingEligibleFails(t *testing.T) {
	b := &multi.BestOfN{Candidates: []harness.Runner{broken("x"), broken("y")}}
	res, err := b.Run(context.Background(), harness.Prompt("q"))
	if res == nil || res.OK() || !strings.Contains(res.Detail, "no candidate") {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestAJudgeThatPicksNothingLeavesTheFirstAnswer(t *testing.T) {
	b := &multi.BestOfN{Candidates: []harness.Runner{bot("a", "one"), bot("b", "two")}, Judge: bot("j", "no idea")}
	if res, err := b.Run(context.Background(), harness.Prompt("q")); err != nil || res.Output != "one" {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestDebateWithoutAJudgeReturnsEveryAnswer(t *testing.T) {
	d := &multi.Debate{Debaters: []harness.Runner{bot("a", "A"), bot("b", "B")}, Rounds: 1}
	res, err := d.Run(context.Background(), harness.Prompt("q"))
	if err != nil || !strings.Contains(res.Output, "A") || !strings.Contains(res.Output, "B") {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestDebatersSeeEachOther(t *testing.T) {
	w := newWorld()
	var heard []string
	a := w.answering("a", func(prompt string, _ []string) (string, string) {
		heard = append(heard, prompt)
		return "from a", ""
	}, nil)
	d := &multi.Debate{Debaters: []harness.Runner{a, w.saying("b", "from b")}}
	if _, err := d.Run(context.Background(), harness.Prompt("q")); err != nil {
		t.Fatal(err)
	}
	if len(heard) != 2 || !strings.Contains(heard[1], "from b") || !strings.Contains(heard[1], "from a") {
		t.Fatalf("%q", heard)
	}
}

func TestRouterFallbackOnClassifierFailure(t *testing.T) {
	r := &multi.Router{Classifier: broken("cls"), Routes: map[string]harness.Runner{"a": bot("a", "A")}, Fallback: bot("fb", "FB")}
	res, err := r.Run(context.Background(), harness.Prompt("q"))
	if err != nil || res.Output != "FB" {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestAnUnroutableRequestWithNoFallbackFails(t *testing.T) {
	r := &multi.Router{Classifier: bot("c", "nonsense"), Routes: map[string]harness.Runner{"a": bot("a", "A")}}
	if res, _ := r.Run(context.Background(), harness.Prompt("q")); res == nil || res.OK() {
		t.Fatalf("%+v", res)
	}
}

// A classifier that waits has not failed, so it must not fall through to Fallback.
func TestAClassifierThatWaitsParksTheRouter(t *testing.T) {
	w := newWorld()
	j := newJournal()
	r := &multi.Router{Journal: j, Classifier: w.paying("c", "ask", asks()), Fallback: w.saying("fb", "fallen")}
	in := harness.Prompt("q")
	in.RunID = "root"
	res, err := r.Run(context.Background(), in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestEvaluatorNoProgressAndCap(t *testing.T) {
	same := &multi.EvaluatorOptimizer{Generator: bot("g", "same"), Evaluator: bot("e", "REVISE: more")}
	res, err := same.Run(context.Background(), harness.Prompt("x"))
	if err != nil || res.Stop != harness.StopLoop || res.Output != "same" {
		t.Fatalf("%v %+v", err, res)
	}
	w := newWorld()
	n := 0
	changing := &multi.EvaluatorOptimizer{MaxRounds: 2, Evaluator: bot("e", "REVISE: more"),
		Generator: w.answering("g", func(string, []string) (string, string) { n++; return strings.Repeat("x", n), "" }, nil)}
	res, err = changing.Run(context.Background(), harness.Prompt("x"))
	if err != nil || res.Stop != harness.StopMaxRepairs || res.Output != "xx" {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestAStoppedGeneratorStopsTheLoop(t *testing.T) {
	eo := &multi.EvaluatorOptimizer{Generator: broken("g"), Evaluator: bot("e", "APPROVED")}
	if res, _ := eo.Run(context.Background(), harness.Prompt("x")); res == nil || res.OK() {
		t.Fatalf("%+v", res)
	}
}

func TestPlannerWorkerSubPlanner(t *testing.T) {
	w := newWorld()
	planner := w.answering("planner", func(p string, _ []string) (string, string) {
		if strings.HasPrefix(p, "Goal:\nfrontend") {
			return "TASK: build the form\nDONE", ""
		}
		return "PLAN: frontend\nTASK: write the API\nDONE", ""
	}, nil)
	worker := w.answering("worker", func(p string, _ []string) (string, string) {
		return "did " + strings.Split(strings.SplitN(p, "Your task:\n", 2)[1], "\n")[0], ""
	}, nil)
	res, err := (&multi.PlannerWorker{Planner: planner, Worker: worker}).Run(context.Background(), harness.Prompt("app"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"did build the form", "did write the API"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("missing %q in %q", want, res.Output)
		}
	}
}

func TestPlannerWorkerCapsTasksAndReportsAFailedOne(t *testing.T) {
	w := newWorld()
	planner := w.answering("planner", func(p string, _ []string) (string, string) {
		if strings.Contains(p, "Report 1") {
			return "DONE", ""
		}
		return "TASK: a\nTASK: b\nTASK: c", ""
	}, nil)
	res, err := (&multi.PlannerWorker{Planner: planner, Worker: broken("w"), MaxTasks: 2, MaxRounds: 2}).Run(context.Background(), harness.Prompt("app"))
	if err != nil || strings.Count(res.Output, "[failed]") != 2 {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestHandoffPingPongIsStopped(t *testing.T) {
	a := &harness.Agent{Name: "a", Provider: fake.NewFunc(func(model.Request) (model.Response, error) { return fake.Call("transfer_to_b", nil), nil })}
	b := &harness.Agent{Name: "b", Provider: fake.NewFunc(func(model.Request) (model.Response, error) { return fake.Call("transfer_to_a", nil), nil })}
	h := &multi.Handoff{Agents: []*harness.Agent{a, b}, MaxHops: 3}
	if res, _ := h.Run(context.Background(), harness.Prompt("x")); res == nil || res.Stop != harness.StopLoop {
		t.Fatalf("%+v", res)
	}
}

// The owner lives in the session's history, so a new process continues with it.
func TestHandoffStickySurvivesARestart(t *testing.T) {
	j := newJournal()
	build := func() *multi.Handoff {
		a := &harness.Agent{Name: "a", Provider: fake.Script(fake.Call("transfer_to_b", nil), fake.Text("a is back"))}
		b := &harness.Agent{Name: "b", Provider: fake.Script(fake.Text("b1"), fake.Text("b2"))}
		return &multi.Handoff{Journal: j, Agents: []*harness.Agent{a, b}, Sticky: true}
	}
	h := build()
	s := harness.NewSession(h)
	first, err := s.Send(context.Background(), "one")
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Owner(first.Messages); got != "b" {
		t.Fatalf("owner %q", got)
	}
	s.Runner = build()
	res, err := s.Send(context.Background(), "two")
	if err != nil || res.Output != "b1" {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestHandoffWithoutStickyStartsOver(t *testing.T) {
	a := &harness.Agent{Name: "a", Provider: fake.Script(fake.Call("transfer_to_b", nil), fake.Text("a again"))}
	b := &harness.Agent{Name: "b", Provider: fake.Script(fake.Text("b1"))}
	s := harness.NewSession(&multi.Handoff{Agents: []*harness.Agent{a, b}})
	var res *harness.Run
	for _, text := range []string{"one", "two"} {
		var err error
		if res, err = s.Send(context.Background(), text); err != nil {
			t.Fatal(err)
		}
	}
	if got := res.Messages[len(res.Messages)-1].Content; got != "a again" {
		t.Fatal(got)
	}
}

// A signature covers the instructions and tools it was made under, so the next owner
// cannot use it.
func TestATransferDropsProviderState(t *testing.T) {
	var seen []model.Message
	a := &harness.Agent{Name: "a", Provider: fake.Script(model.Response{Opaque: json.RawMessage(`{"anthropic":[]}`),
		ToolCalls: []model.ToolCall{{ID: "c", Name: "transfer_to_b", Arguments: json.RawMessage(`{"reason":"yours"}`)}}})}
	b := &harness.Agent{Name: "b", Provider: fake.NewFunc(func(req model.Request) (model.Response, error) {
		seen = req.Messages
		return fake.Text("ok"), nil
	})}
	if _, err := (&multi.Handoff{Agents: []*harness.Agent{a, b}}).Run(context.Background(), harness.Prompt("x")); err != nil {
		t.Fatal(err)
	}
	if len(seen) < 3 || !strings.Contains(seen[len(seen)-1].Content, "Note: yours") {
		t.Fatalf("%+v", seen)
	}
	for _, m := range seen {
		if len(m.Opaque) != 0 {
			t.Fatalf("stale state: %+v", m)
		}
	}
}

func TestHandoffRefusesAToolThatClashesWithATransfer(t *testing.T) {
	clash := tool.Func(tool.Spec{Name: "transfer_to_b"}, func(context.Context, json.RawMessage) (string, error) { return "", nil })
	a := &harness.Agent{Name: "a", Tools: tool.NewRegistry(clash), Provider: fake.Script()}
	b := &harness.Agent{Name: "b", Provider: fake.Script()}
	if _, err := (&multi.Handoff{Agents: []*harness.Agent{a, b}}).Run(context.Background(), harness.Prompt("x")); err == nil {
		t.Fatal("want an error")
	}
}

func teamOf(w *world, names ...string) []harness.Runner {
	var out []harness.Runner
	for _, n := range names {
		out = append(out, w.answering(n, func(prompt string, _ []string) (string, string) {
			task := strings.Split(strings.SplitN(prompt, "):\n", 2)[1], "\n")[0]
			if task == "root" {
				return "NEW TASK: child\nNEW TASK: child two\nNEW TASK: child three", ""
			}
			return n + " did " + task, ""
		}, nil))
	}
	return out
}

func TestTeamAssignsByPositionAndWorksThroughFollowUps(t *testing.T) {
	tm := &multi.Team{Members: teamOf(newWorld(), "m1", "m2"), Seed: func(harness.Input) []string { return []string{"root"} }, MaxTasks: 3}
	res, err := tm.Run(context.Background(), harness.Prompt("goal"))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	for _, want := range []string{"Task 2 (done, m2#2): child", "m2 did child", "Task 3 (done, m1#1): child two", "m1 did child two"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("missing %q in\n%s", want, res.Output)
		}
	}
	if strings.Contains(res.Output, "Task 4") {
		t.Fatalf("over the cap:\n%s", res.Output)
	}
}

func TestAFailedTaskIsReportedAndTheRestFinish(t *testing.T) {
	tm := &multi.Team{Members: []harness.Runner{broken("bad"), bot("good", "fine")}, Seed: func(harness.Input) []string { return []string{"a", "b"} }}
	res, err := tm.Run(context.Background(), harness.Prompt("goal"))
	if res == nil || res.Stop != harness.StopFailed || !strings.Contains(res.Output, "fine") || !strings.Contains(res.Detail, "1 tasks failed") {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestATeamNeedsMembers(t *testing.T) {
	if _, err := (&multi.Team{}).Run(context.Background(), harness.Prompt("goal")); err == nil {
		t.Fatal("want an error")
	}
}

func TestASeedOverTheCapIsCut(t *testing.T) {
	tm := &multi.Team{Members: []harness.Runner{bot("m", "ok")}, MaxTasks: 1, Seed: func(harness.Input) []string { return []string{"a", "b"} }}
	res, err := tm.Run(context.Background(), harness.Prompt("goal"))
	if err != nil || strings.Contains(res.Output, "Task 2") {
		t.Fatalf("%v %+v", err, res)
	}
}

// A child parked deep in a pattern's rounds is reached by the walk, so a cancel leaves
// no pattern with a wait open below it.
func TestCancelEndsAParkedChildInEveryPattern(t *testing.T) {
	type built struct {
		name  string
		child string
		build func(w *world, j harness.Journal) harness.Runner
	}
	parked := func(w *world, name string) *harness.Agent { return w.paying(name, "send", asks()) }
	for _, c := range []built{
		{"best of n", "root/1", func(w *world, j harness.Journal) harness.Runner {
			return &multi.BestOfN{Journal: j, Candidates: []harness.Runner{w.saying("a", "1"), parked(w, "b")}}
		}},
		{"debate, second round", "root/1/1", func(w *world, j harness.Journal) harness.Runner {
			b := w.answering("b", func(p string, used []string) (string, string) {
				if strings.Contains(p, "Reconsider") && len(used) == 0 {
					return "", "send"
				}
				return "said", ""
			}, []harness.Hook{asks()}, "send")
			return &multi.Debate{Journal: j, Debaters: []harness.Runner{w.saying("a", "1"), b}}
		}},
		{"evaluator, its judge", "root/evaluate/0", func(w *world, j harness.Journal) harness.Runner {
			return &multi.EvaluatorOptimizer{Journal: j, Generator: w.saying("g", "draft"), Evaluator: parked(w, "e")}
		}},
		{"router, a route", "root/route/a", func(w *world, j harness.Journal) harness.Runner {
			return &multi.Router{Journal: j, Classifier: w.saying("c", "a"), Routes: map[string]harness.Runner{"a": parked(w, "a")}}
		}},
		{"planner, a worker", "root/work/0/0", func(w *world, j harness.Journal) harness.Runner {
			return &multi.PlannerWorker{Journal: j, Planner: w.saying("p", "TASK: x"), Worker: parked(w, "w")}
		}},
		{"planner, inside a sub-planner", "root/sub/0/0/work/0/0", func(w *world, j harness.Journal) harness.Runner {
			planner := w.answering("p", func(prompt string, _ []string) (string, string) {
				if strings.HasPrefix(prompt, "Goal:\nslice") {
					return "TASK: x", ""
				}
				return "PLAN: slice", ""
			}, nil)
			return &multi.PlannerWorker{Journal: j, Planner: planner, Worker: parked(w, "w")}
		}},
		{"handoff, the second owner", "root/hop/1/b", func(w *world, j harness.Journal) harness.Runner {
			a := w.answering("a", func(string, []string) (string, string) { return "", "transfer_to_b" }, nil)
			return &multi.Handoff{Journal: j, Agents: []*harness.Agent{a, parked(w, "b")}}
		}},
		{"team, a follow-up", "root/task/1", func(w *world, j harness.Journal) harness.Runner {
			return &multi.Team{Journal: j, Members: []harness.Runner{w.saying("m1", "NEW TASK: more"), parked(w, "m2")}}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			w := newWorld()
			j := newJournal()
			in := harness.Prompt("x")
			in.RunID = "root"
			res, err := c.build(w, j).Run(ctx, in)
			if err != nil || res.State != harness.StateSuspended {
				t.Fatalf("%v %+v", err, res)
			}
			if !slices.Contains(j.ids(), c.child) {
				t.Fatalf("no %s in %v", c.child, j.ids())
			}
			if len(res.Waits) != 1 || res.Waits[0].Run != c.child {
				t.Fatalf("%+v", res.Waits)
			}
			paid := w.billed()
			cancel(t, c.build(w, j), "root")
			cancelled(t, j, c.child)
			if w.count("send") != 0 || w.billed() != paid {
				t.Fatalf("send ran %d times, paid %d more", w.count("send"), w.billed()-paid)
			}
		})
	}
}

// A planner's output decides the tree, so changing a part must not hand a child's
// journal to something else.
func TestAReorderedTeamFailsARunInFlight(t *testing.T) {
	ctx := context.Background()
	w := newWorld()
	j := newJournal()
	build := func(names ...string) *multi.Team {
		var ms []harness.Runner
		for _, n := range names {
			ms = append(ms, w.paying(n, "send", asks()))
		}
		return &multi.Team{Journal: j, Members: ms}
	}
	in := harness.Prompt("x")
	in.RunID = "root"
	if res, err := build("a", "b").Run(ctx, in); err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	sig := harness.Signal{Wait: fold(t, j, "root").Waits[0].ID, Value: "yes"}
	res, err := build("b", "a").Run(ctx, harness.Resume("root", sig))
	if !errors.Is(err, harness.ErrFenced) && (res == nil || res.State != harness.StateFailed) {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestASingleEligibleCandidateNeedsNoJudge(t *testing.T) {
	w := newWorld()
	b := &multi.BestOfN{Candidates: []harness.Runner{broken("x"), w.saying("a", "only")}, Judge: w.saying("j", "WINNER: 1")}
	res, err := b.Run(context.Background(), harness.Prompt("q"))
	if err != nil || res.Output != "only" || w.billed() != 1 {
		t.Fatalf("%v %+v, billed %d", err, res, w.billed())
	}
}

func TestCancelEndsAParkedJudge(t *testing.T) {
	ctx := context.Background()
	w := newWorld()
	j := newJournal()
	build := func() *multi.BestOfN {
		return &multi.BestOfN{Journal: j, Candidates: []harness.Runner{w.saying("a", "1"), w.saying("b", "2")}, Judge: w.paying("judge", "send", asks())}
	}
	in := harness.Prompt("q")
	in.RunID = "root"
	if res, err := build().Run(ctx, in); err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	cancel(t, build(), "root")
	cancelled(t, j, "root/judge")
}

// Without the cap a planner that always delegates would recurse forever.
func TestPlansBelowTheDepthCapBecomeTasks(t *testing.T) {
	w := newWorld()
	planner := w.saying("p", "PLAN: again")
	var tasks []string
	worker := w.answering("w", func(prompt string, _ []string) (string, string) {
		tasks = append(tasks, prompt)
		return "done", ""
	}, nil)
	pw := &multi.PlannerWorker{Planner: planner, Worker: worker, MaxRounds: 1, MaxDepth: 1}
	if res, err := pw.Run(context.Background(), harness.Prompt("goal")); err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if len(tasks) != 1 || !strings.Contains(tasks[0], "Your task:\nagain") {
		t.Fatalf("%q", tasks)
	}
	if got := w.billed(); got != 3 { // the planner, one sub-planner, one worker
		t.Fatalf("%d model calls", got)
	}
}

func TestDoneEndsThePlanWithoutAnotherRound(t *testing.T) {
	w := newWorld()
	calls := 0
	planner := w.answering("p", func(string, []string) (string, string) {
		calls++
		return "TASK: last\nDONE", ""
	}, nil)
	if _, err := (&multi.PlannerWorker{Planner: planner, Worker: w.saying("w", "ok")}).Run(context.Background(), harness.Prompt("goal")); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("planned %d times", calls)
	}
}

func TestAJudgeThatSaysDoneEndsThePlan(t *testing.T) {
	w := newWorld()
	calls := 0
	planner := w.answering("p", func(string, []string) (string, string) {
		calls++
		return "TASK: more", ""
	}, nil)
	pw := &multi.PlannerWorker{Planner: planner, Worker: w.saying("w", "ok"), Judge: w.saying("j", "DONE, that is enough")}
	if _, err := pw.Run(context.Background(), harness.Prompt("goal")); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("planned %d times", calls)
	}
}

func TestReviewerFeedbackReachesTheGenerator(t *testing.T) {
	w := newWorld()
	var heard string
	gen := w.answering("g", func(prompt string, _ []string) (string, string) {
		heard = prompt
		return fmt.Sprint(len(prompt)), ""
	}, nil)
	eo := &multi.EvaluatorOptimizer{MaxRounds: 2, Generator: gen, Evaluator: bot("e", "REVISE: add a title")}
	if _, err := eo.Run(context.Background(), harness.Prompt("x")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(heard, "add a title") {
		t.Fatalf("%q", heard)
	}
}

// A transfer has no effect, so a crash inside one must not ask a person what happened.
func TestACrashInATransferAsksNobody(t *testing.T) {
	ctx := context.Background()
	build := func(j harness.Journal) *multi.Handoff {
		a := &harness.Agent{Name: "a", Provider: fake.NewFunc(func(model.Request) (model.Response, error) { return fake.Call("transfer_to_b", nil), nil })}
		b := &harness.Agent{Name: "b", Provider: fake.NewFunc(func(model.Request) (model.Response, error) { return fake.Text("mine now"), nil })}
		return &multi.Handoff{Journal: j, Agents: []*harness.Agent{a, b}}
	}
	in := harness.Prompt("x")
	in.RunID = "root"
	for at := 1; ; at++ {
		j := newJournal()
		_, err := build(kill(j, "root/hop/0/a", at, true)).Run(ctx, in)
		if err == nil {
			if at < 4 {
				t.Fatalf("only %d appends", at)
			}
			return
		}
		res, err := build(j).Run(ctx, harness.Resume("root"))
		if err != nil || res.Output != "mine now" {
			t.Fatalf("append %d: %v %+v", at, err, res)
		}
	}
}

func TestAStoppedChildKeepsItsPartialOutput(t *testing.T) {
	long := &harness.Agent{Name: "long", Provider: fake.Script(model.Response{Text: "half an answer", Finish: model.FinishLength})}
	r := &multi.Router{Classifier: bot("c", "a"), Routes: map[string]harness.Runner{"a": long}}
	res, err := r.Run(context.Background(), harness.Prompt("x"))
	if err != nil || res.Stop != harness.StopTruncated || res.Output != "half an answer" {
		t.Fatalf("%v %+v", err, res)
	}
}

// A shape pins what the run is made of. Rounds and hops repeat the same runners, and
// an in-flight run must not fail because a limit moved.
func TestAShapeNamesRolesNotRounds(t *testing.T) {
	j := newJournal()
	in := harness.Prompt("x")
	in.RunID = "root"
	d := &multi.Debate{Journal: j, Debaters: []harness.Runner{bot("a", "1"), bot("b", "2")}, Rounds: 3}
	if _, err := d.Run(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if got := fold(t, j, "root").Pin.Shape; got != "debate: 0:a, 1:b" {
		t.Fatal(got)
	}
}
