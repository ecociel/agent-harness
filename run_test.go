package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

// Short names for the state path assertions.
const (
	st = harness.StateStart
	pr = harness.StatePrepare
	co = harness.StateCompose
	mo = harness.StateModel
	ro = harness.StateRoute
	to = harness.StateTools
	ob = harness.StateObserve
	ve = harness.StateVerify
	dn = harness.StateDone
	sp = harness.StateStopped
	fl = harness.StateFailed
)

func echoTool() tool.Tool {
	return tool.Func(tool.Spec{Name: "echo"}, func(_ context.Context, a json.RawMessage) (string, error) {
		return "echo " + string(a), nil
	})
}

func handoffTool() tool.Tool {
	return tool.Func(tool.Spec{Name: "transfer"}, func(context.Context, json.RawMessage) (string, error) {
		return "", &tool.Handoff{Target: "billing", Note: "invoice question"}
	})
}

func failTool() tool.Tool {
	return tool.Func(tool.Spec{Name: "broken"}, func(context.Context, json.RawMessage) (string, error) {
		return "", errors.New("disk on fire")
	})
}

// testHook is a configurable hook implementing every point.
type testHook struct {
	name      string
	log       *[]string
	beforeRun error
	model     error
	decision  harness.Decision
	verdicts  []harness.Verdict
	turnErr   error
}

func (h *testHook) Name() string { return h.name }
func (h *testHook) rec(s string) {
	if h.log != nil {
		*h.log = append(*h.log, h.name+"."+s)
	}
}
func (h *testHook) BeforeRun(context.Context, *harness.Run) error {
	h.rec("BeforeRun")
	return h.beforeRun
}
func (h *testHook) BeforeModel(context.Context, *harness.Run, *model.Request) error {
	h.rec("BeforeModel")
	return h.model
}
func (h *testHook) WrapModel(_ *harness.Run, next harness.ModelCall) harness.ModelCall {
	return func(ctx context.Context, req model.Request) (model.Response, error) {
		h.rec("WrapModel>")
		r, err := next(ctx, req)
		h.rec("WrapModel<")
		return r, err
	}
}
func (h *testHook) AfterModel(context.Context, *harness.Run, *model.Response) error {
	h.rec("AfterModel")
	return nil
}
func (h *testHook) BeforeTool(context.Context, *harness.Run, model.ToolCall) harness.Decision {
	h.rec("BeforeTool")
	return h.decision
}
func (h *testHook) WrapTool(_ *harness.Run, next harness.ToolCallFunc) harness.ToolCallFunc {
	return func(ctx context.Context, c model.ToolCall) (string, error) {
		h.rec("WrapTool")
		return next(ctx, c)
	}
}
func (h *testHook) AfterTool(context.Context, *harness.Run, harness.ToolOutcome) error {
	h.rec("AfterTool")
	return nil
}
func (h *testHook) AfterTurn(context.Context, *harness.Run) error {
	h.rec("AfterTurn")
	return h.turnErr
}
func (h *testHook) CheckOutput(context.Context, *harness.Run, string) harness.Verdict {
	h.rec("CheckOutput")
	if len(h.verdicts) == 0 {
		return harness.Accept()
	}
	v := h.verdicts[0]
	h.verdicts = h.verdicts[1:]
	return v
}
func (h *testHook) AfterRun(context.Context, *harness.Run, *harness.Result) { h.rec("AfterRun") }

func TestRunPaths(t *testing.T) {
	tests := []struct {
		name      string
		script    []model.Response
		tools     []tool.Tool
		hook      *testHook
		maxTurns  int
		wantPath  []harness.State
		wantStop  harness.StopReason
		wantOut   string
		wantError bool
	}{
		{
			name:     "plain answer",
			script:   []model.Response{fake.Text("hi")},
			wantPath: []harness.State{st, pr, co, mo, ro, ve, dn},
			wantStop: harness.StopCompleted, wantOut: "hi",
		},
		{
			name:     "one tool turn",
			script:   []model.Response{fake.Call("echo", map[string]int{"x": 1}), fake.Text("done")},
			tools:    []tool.Tool{echoTool()},
			wantPath: []harness.State{st, pr, co, mo, ro, to, ob, co, mo, ro, ve, dn},
			wantStop: harness.StopCompleted, wantOut: "done",
		},
		{
			name:     "tool denied is fed back and the run continues",
			script:   []model.Response{fake.Call("echo", nil), fake.Text("ok without it")},
			tools:    []tool.Tool{echoTool()},
			hook:     &testHook{decision: harness.Deny("not today")},
			wantPath: []harness.State{st, pr, co, mo, ro, to, ob, co, mo, ro, ve, dn},
			wantStop: harness.StopCompleted, wantOut: "ok without it",
		},
		{
			name:     "tool halt stops the run",
			script:   []model.Response{fake.Call("echo", nil)},
			tools:    []tool.Tool{echoTool()},
			hook:     &testHook{decision: harness.Halt("forbidden")},
			wantPath: []harness.State{st, pr, co, mo, ro, to, sp},
			wantStop: harness.StopGuardrail,
		},
		{
			name:     "input guardrail stops in prepare",
			script:   []model.Response{fake.Text("never")},
			hook:     &testHook{beforeRun: harness.Stop(harness.StopGuardrail, "pii")},
			wantPath: []harness.State{st, pr, sp},
			wantStop: harness.StopGuardrail,
		},
		{
			name:     "budget stop before model",
			script:   []model.Response{fake.Text("never")},
			hook:     &testHook{model: harness.Stop(harness.StopBudget, "0 tokens left")},
			wantPath: []harness.State{st, pr, co, sp},
			wantStop: harness.StopBudget,
		},
		{
			name:     "revise loops back to compose",
			script:   []model.Response{fake.Text("draft"), fake.Text("final")},
			hook:     &testHook{verdicts: []harness.Verdict{harness.Revise("too short")}},
			wantPath: []harness.State{st, pr, co, mo, ro, ve, co, mo, ro, ve, dn},
			wantStop: harness.StopCompleted, wantOut: "final",
		},
		{
			name:   "repairs are capped",
			script: []model.Response{fake.Text("a"), fake.Text("b"), fake.Text("c")},
			hook: &testHook{verdicts: []harness.Verdict{harness.Revise("no"),
				harness.Revise("no"), harness.Revise("still no")}},
			wantPath: []harness.State{st, pr, co, mo, ro, ve, co, mo, ro, ve, co, mo, ro, ve, sp},
			wantStop: harness.StopMaxRepairs, wantOut: "c",
		},
		{
			name:     "output block",
			script:   []model.Response{fake.Text("secret")},
			hook:     &testHook{verdicts: []harness.Verdict{harness.Block("leak")}},
			wantPath: []harness.State{st, pr, co, mo, ro, ve, sp},
			wantStop: harness.StopGuardrail,
		},
		{
			name:     "rewrite redacts",
			script:   []model.Response{fake.Text("card 4111")},
			hook:     &testHook{verdicts: []harness.Verdict{{Rewrite: "card ****"}}},
			wantPath: []harness.State{st, pr, co, mo, ro, ve, dn},
			wantStop: harness.StopCompleted, wantOut: "card ****",
		},
		{
			name:     "max turns",
			script:   []model.Response{fake.Call("echo", nil), fake.Call("echo", nil)},
			tools:    []tool.Tool{echoTool()},
			maxTurns: 1,
			wantPath: []harness.State{st, pr, co, mo, ro, to, ob, co, sp},
			wantStop: harness.StopMaxTurns,
		},
		{
			name:     "after turn stop (loop detection)",
			script:   []model.Response{fake.Call("echo", nil)},
			tools:    []tool.Tool{echoTool()},
			hook:     &testHook{turnErr: harness.Stop(harness.StopLoop, "same call 3x")},
			wantPath: []harness.State{st, pr, co, mo, ro, to, ob, sp},
			wantStop: harness.StopLoop,
		},
		{
			name:     "handoff",
			script:   []model.Response{fake.Call("transfer", nil)},
			tools:    []tool.Tool{handoffTool()},
			wantPath: []harness.State{st, pr, co, mo, ro, to, sp},
			wantStop: harness.StopHandoff,
		},
		{
			name:     "tool error is reported to the model",
			script:   []model.Response{fake.Call("broken", nil), fake.Text("recovered")},
			tools:    []tool.Tool{failTool()},
			wantPath: []harness.State{st, pr, co, mo, ro, to, ob, co, mo, ro, ve, dn},
			wantStop: harness.StopCompleted, wantOut: "recovered",
		},
		{
			name:      "provider failure",
			script:    nil,
			wantPath:  []harness.State{st, pr, co, mo, fl},
			wantStop:  harness.StopFailed,
			wantError: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &harness.Agent{Name: "t", Provider: fake.Script(tt.script...),
				Tools: tool.NewRegistry(tt.tools...), MaxTurns: tt.maxTurns}
			if tt.hook != nil {
				tt.hook.name = "h"
				a.Hooks = []harness.Hook{tt.hook}
			}
			res, err := a.Run(context.Background(), harness.Prompt("go"))
			if (err != nil) != tt.wantError {
				t.Fatalf("err = %v", err)
			}
			if !reflect.DeepEqual(res.Path, tt.wantPath) {
				t.Errorf("path\n got %v\nwant %v", res.Path, tt.wantPath)
			}
			if res.Stop != tt.wantStop {
				t.Errorf("stop = %s, want %s (%s)", res.Stop, tt.wantStop, res.Detail)
			}
			if res.Output != tt.wantOut {
				t.Errorf("output = %q, want %q", res.Output, tt.wantOut)
			}
		})
	}
}

func TestHookOrderAndMiddlewareNesting(t *testing.T) {
	var log []string
	a := &harness.Agent{
		Provider: fake.Script(fake.Call("echo", nil), fake.Text("ok")),
		Tools:    tool.NewRegistry(echoTool()),
		Hooks:    []harness.Hook{&testHook{name: "a", log: &log}, &testHook{name: "b", log: &log}},
	}
	if _, err := a.Run(context.Background(), harness.Prompt("x")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"a.BeforeRun", "b.BeforeRun",
		"a.BeforeModel", "b.BeforeModel", "a.WrapModel>", "b.WrapModel>", "b.WrapModel<", "a.WrapModel<",
		"a.AfterModel", "b.AfterModel",
		"a.BeforeTool", "b.BeforeTool", "a.WrapTool", "b.WrapTool", "a.AfterTool", "b.AfterTool",
		"a.AfterTurn", "b.AfterTurn",
		"a.BeforeModel", "b.BeforeModel", "a.WrapModel>", "b.WrapModel>", "b.WrapModel<", "a.WrapModel<",
		"a.AfterModel", "b.AfterModel",
		"a.CheckOutput", "b.CheckOutput",
		"a.AfterRun", "b.AfterRun",
	}
	if !reflect.DeepEqual(log, want) {
		t.Errorf("order\n got %v\nwant %v", log, want)
	}
}

func TestEventsCoverTransitionsAndHooks(t *testing.T) {
	var (
		mu     sync.Mutex
		events []harness.Event
	)
	ctx := harness.WithEvents(context.Background(), func(e harness.Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	})
	a := &harness.Agent{Name: "ev", Provider: fake.Script(fake.Call("echo", nil), fake.Text("fine")),
		Tools: tool.NewRegistry(echoTool()), Hooks: []harness.Hook{&testHook{name: "h"}}}
	res, err := a.Run(ctx, harness.Prompt("x"))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[harness.EventKind]int{}
	points := map[harness.Point]bool{}
	for _, e := range events {
		kinds[e.Kind]++
		if e.Kind == harness.KindHook {
			points[e.Point] = true
		}
		if e.RunID != res.RunID {
			t.Fatalf("event from other run: %+v", e)
		}
	}
	if kinds[harness.KindTransition] != len(res.Path)-1 {
		t.Errorf("transitions = %d, path = %d", kinds[harness.KindTransition], len(res.Path))
	}
	for _, p := range []harness.Point{harness.PointBeforeRun, harness.PointBeforeModel, harness.PointWrapModel,
		harness.PointAfterModel, harness.PointBeforeTool, harness.PointWrapTool, harness.PointAfterTool,
		harness.PointAfterTurn, harness.PointCheckOutput, harness.PointAfterRun} {
		if !points[p] {
			t.Errorf("no hook event for %s", p)
		}
	}
	if kinds[harness.KindRunStart] != 1 || kinds[harness.KindRunEnd] != 1 || kinds[harness.KindDelta] == 0 {
		t.Errorf("kinds = %v", kinds)
	}
}

func TestSubAgentDepthAndParent(t *testing.T) {
	var mu sync.Mutex
	depths := map[string]int{}
	parents := map[string]string{}
	ctx := harness.WithEvents(context.Background(), func(e harness.Event) {
		if e.Kind == harness.KindRunStart {
			mu.Lock()
			depths[e.Agent], parents[e.Agent] = e.Depth, e.ParentRun
			mu.Unlock()
		}
	})
	child := &harness.Agent{Name: "child", Provider: fake.Script(fake.Text("child says hi"))}
	delegate := tool.Func(tool.Spec{Name: "ask_child"}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		if harness.Depth(ctx) != 1 {
			return "", fmt.Errorf("depth = %d", harness.Depth(ctx))
		}
		r, err := child.Run(ctx, harness.Prompt("hi"))
		if err != nil {
			return "", err
		}
		return r.Output, nil
	})
	parent := &harness.Agent{Name: "parent", Provider: fake.Script(fake.Call("ask_child", nil), fake.Text("done")),
		Tools: tool.NewRegistry(delegate)}
	res, err := parent.Run(ctx, harness.Prompt("go"))
	if err != nil || !res.OK() {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if depths["parent"] != 0 || depths["child"] != 1 || parents["child"] != res.RunID {
		t.Fatalf("depths=%v parents=%v", depths, parents)
	}
	if !strings.Contains(res.Messages[2].Content, "child says hi") {
		t.Fatalf("tool result = %q", res.Messages[2].Content)
	}
}

func TestCancellationStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := &harness.Agent{Provider: fake.Script(fake.Text("x"))}
	res, err := a.Run(ctx, harness.Prompt("x"))
	if err != nil || res.Stop != harness.StopCancelled {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestInstructionsAreSentButNotStored(t *testing.T) {
	p := fake.Script(fake.Text("ok"))
	a := &harness.Agent{Instructions: "be brief", Provider: p}
	res, _ := a.Run(context.Background(), harness.Prompt("hi"))
	req := p.Requests()[0]
	if req.Messages[0].Role != model.RoleSystem || req.Messages[0].Content != "be brief" {
		t.Fatalf("request = %+v", req.Messages)
	}
	for _, m := range res.Messages {
		if m.Role == model.RoleSystem {
			t.Fatal("system message leaked into history")
		}
	}
}

func TestSessionCarriesHistory(t *testing.T) {
	p := fake.Script(fake.Text("one"), fake.Text("two"))
	s := harness.NewSession(&harness.Agent{Provider: p})
	if _, err := s.Send(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	if got := len(p.Requests()[1].Messages); got != 3 {
		t.Fatalf("second request has %d messages, want 3", got)
	}
	if len(s.History()) != 4 {
		t.Fatalf("history = %v", s.History())
	}
}
