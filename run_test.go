package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

const (
	st = harness.StateStart
	co = harness.StateCompose
	mo = harness.StateModel
	ga = harness.StateGate
	to = harness.StateTools
	ve = harness.StateVerify
	cl = harness.StateClosing
	dn = harness.StateDone
	sp = harness.StateStopped
	fl = harness.StateFailed
)

func echoTool() tool.Tool {
	return tool.Func(tool.Spec{Name: "echo", Effect: tool.EffectRead}, func(_ context.Context, a json.RawMessage) (string, error) {
		return "echo " + string(a), nil
	})
}

// testHook is a configurable hook.
type testHook struct {
	beforeRun error
	context   error
	decision  harness.Decision
	verdicts  []harness.Verdict
}

func (h *testHook) Name() string { return "test" }
func (h *testHook) BeforeRun(context.Context, harness.Run) error {
	return h.beforeRun
}
func (h *testHook) Context(context.Context, harness.Run) (harness.Edit, error) {
	return harness.Edit{}, h.context
}
func (h *testHook) BeforeTool(context.Context, harness.Run, harness.Call) harness.Decision {
	return h.decision
}
func (h *testHook) CheckOutput(context.Context, harness.Run, string) harness.Verdict {
	if len(h.verdicts) == 0 {
		return harness.Accept()
	}
	v := h.verdicts[0]
	h.verdicts = h.verdicts[1:]
	return v
}

func TestRunPaths(t *testing.T) {
	echo := fake.Call("echo", map[string]string{"x": "1"})
	tests := []struct {
		name   string
		script []model.Response
		hook   testHook
		max    int
		path   []harness.State
		stop   harness.StopReason
		output string
	}{
		{name: "plain answer", script: []model.Response{fake.Text("hi")},
			path: []harness.State{st, co, mo, ve, cl, dn}, stop: harness.StopCompleted, output: "hi"},
		{name: "tool turn", script: []model.Response{echo, fake.Text("hi")},
			path: []harness.State{st, co, mo, ga, to, co, mo, ve, cl, dn}, stop: harness.StopCompleted, output: "hi"},
		{name: "tool denied", script: []model.Response{echo, fake.Text("ok")}, hook: testHook{decision: harness.Deny("no")},
			path: []harness.State{st, co, mo, ga, co, mo, ve, cl, dn}, stop: harness.StopCompleted, output: "ok"},
		{name: "tool halt", script: []model.Response{echo}, hook: testHook{decision: harness.Halt("stop")},
			path: []harness.State{st, co, mo, ga, cl, sp}, stop: harness.StopGuardrail},
		{name: "context stop", script: []model.Response{fake.Text("x")}, hook: testHook{context: harness.Stop(harness.StopBudget, "out")},
			path: []harness.State{st, co, cl, sp}, stop: harness.StopBudget},
		{name: "revise", script: []model.Response{fake.Text("a"), fake.Text("b")}, hook: testHook{verdicts: []harness.Verdict{harness.Revise("again")}},
			path: []harness.State{st, co, mo, ve, co, mo, ve, cl, dn}, stop: harness.StopCompleted, output: "b"},
		{name: "repairs capped", script: []model.Response{fake.Text("a"), fake.Text("b"), fake.Text("c")},
			hook: testHook{verdicts: []harness.Verdict{harness.Revise("1"), harness.Revise("2"), harness.Revise("3")}},
			path: []harness.State{st, co, mo, ve, co, mo, ve, co, mo, ve, cl, sp}, stop: harness.StopMaxRepairs, output: "c"},
		{name: "block", script: []model.Response{fake.Text("bad")}, hook: testHook{verdicts: []harness.Verdict{harness.Block("no")}},
			path: []harness.State{st, co, mo, ve, cl, sp}, stop: harness.StopGuardrail},
		{name: "rewrite", script: []model.Response{fake.Text("secret")}, hook: testHook{verdicts: []harness.Verdict{{Rewrite: "clean"}}},
			path: []harness.State{st, co, mo, ve, cl, dn}, stop: harness.StopCompleted, output: "clean"},
		{name: "max turns", script: []model.Response{echo, echo}, max: 1,
			path: []harness.State{st, co, mo, ga, to, co, cl, sp}, stop: harness.StopMaxTurns},
		{name: "refusal", script: []model.Response{{Text: "I can't", Finish: model.FinishRefusal}},
			path: []harness.State{st, co, mo, cl, sp}, stop: harness.StopRefused},
		{name: "content filter with calls", script: []model.Response{{Finish: model.FinishContentFilter, ToolCalls: echo.ToolCalls}},
			path: []harness.State{st, co, mo, cl, sp}, stop: harness.StopRefused},
		{name: "truncated answer", script: []model.Response{{Text: "half", Finish: model.FinishLength}},
			path: []harness.State{st, co, mo, cl, sp}, stop: harness.StopTruncated, output: "half"},
		{name: "truncated tool arguments are never executed",
			script: []model.Response{{ToolCalls: echo.ToolCalls, Finish: model.FinishLength}, fake.Text("sorry")},
			path:   []harness.State{st, co, mo, co, mo, ve, cl, dn}, stop: harness.StopCompleted, output: "sorry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.hook
			a := &harness.Agent{Provider: fake.Script(tt.script...), Tools: tool.NewRegistry(echoTool()),
				Hooks: []harness.Hook{&h}, MaxTurns: tt.max}
			res, err := a.Run(context.Background(), harness.Prompt("hello"))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(res.Path, tt.path) {
				t.Errorf("path = %v, want %v", res.Path, tt.path)
			}
			if res.Stop != tt.stop || res.Output != tt.output {
				t.Errorf("stop = %s, output = %q", res.Stop, res.Output)
			}
			balanced(t, res.Messages)
		})
	}
}

func TestProviderFailureIsClassified(t *testing.T) {
	boom := &model.Error{Class: model.ClassInvalid, Err: errors.New("bad request")}
	a := &harness.Agent{Provider: model.ProviderFunc(func(context.Context, model.Request) iter.Seq2[model.Event, error] {
		return model.Fail(boom)
	})}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err == nil || res.State != fl || res.Class != model.ClassInvalid {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestToolErrorGoesToTheModel(t *testing.T) {
	broken := tool.Func(tool.Spec{Name: "broken"}, func(context.Context, json.RawMessage) (string, error) {
		return "", errors.New("disk on fire")
	})
	a := &harness.Agent{Provider: fake.Script(fake.Call("broken", nil), fake.Text("sorry")), Tools: tool.NewRegistry(broken)}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil || !res.OK() || !strings.Contains(res.Messages[2].Content, "disk on fire") {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestHaltMidBatchAnswersEveryCall(t *testing.T) {
	calls := []model.ToolCall{{ID: "1", Name: "echo", Arguments: json.RawMessage(`{}`)},
		{ID: "2", Name: "echo", Arguments: json.RawMessage(`{}`)}, {ID: "3", Name: "echo", Arguments: json.RawMessage(`{}`)}}
	a := &harness.Agent{Provider: fake.Script(model.Response{ToolCalls: calls}), Tools: tool.NewRegistry(echoTool()),
		Hooks: []harness.Hook{&testHook{decision: harness.Halt("no")}}}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil {
		t.Fatal(err)
	}
	balanced(t, res.Messages)
	if len(res.Messages) != 5 {
		t.Fatalf("messages: %+v", res.Messages)
	}
}

func TestHandoffMidBatchAnswersEveryCall(t *testing.T) {
	transfer := tool.Func(tool.Spec{Name: "transfer"}, func(context.Context, json.RawMessage) (string, error) {
		return "", &tool.Handoff{Target: "billing", Note: "invoice"}
	})
	calls := []model.ToolCall{{ID: "1", Name: "transfer", Arguments: json.RawMessage(`{}`)}, {ID: "2", Name: "echo", Arguments: json.RawMessage(`{}`)}}
	a := &harness.Agent{Provider: fake.Script(model.Response{ToolCalls: calls}), Tools: tool.NewRegistry(transfer, echoTool())}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil || res.Stop != harness.StopHandoff || res.Handoff.Target != "billing" {
		t.Fatalf("%v %+v", err, res)
	}
	balanced(t, res.Messages)
}

func TestAdjacentReadsRunInParallelAndWritesDoNot(t *testing.T) {
	var live, peak atomic.Int64
	read := func(name string) tool.Tool {
		return tool.Func(tool.Spec{Name: name, Effect: tool.EffectRead}, func(context.Context, json.RawMessage) (string, error) {
			n := live.Add(1)
			defer live.Add(-1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			return name, nil
		})
	}
	var order []string
	write := tool.Func(tool.Spec{Name: "w"}, func(context.Context, json.RawMessage) (string, error) {
		if live.Load() != 0 {
			t.Error("a write ran alongside a read")
		}
		order = append(order, "w")
		return "w", nil
	})
	mk := func(id, name string) model.ToolCall {
		return model.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(`{}`)}
	}
	a := &harness.Agent{Provider: fake.Script(model.Response{ToolCalls: []model.ToolCall{
		mk("1", "r1"), mk("2", "r2"), mk("3", "w"), mk("4", "r3"),
	}}, fake.Text("ok")), Tools: tool.NewRegistry(read("r1"), read("r2"), read("r3"), write)}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if peak.Load() != 2 {
		t.Fatalf("peak concurrency %d, want 2", peak.Load())
	}
	want := []string{"r1", "r2", "w", "r3"}
	for i, name := range want {
		if res.Messages[2+i].Name != name {
			t.Fatalf("results out of model order: %+v", res.Messages)
		}
	}
}

func TestPerCallTimeout(t *testing.T) {
	slow := tool.Func(tool.Spec{Name: "slow", Timeout: time.Millisecond}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	a := &harness.Agent{Provider: fake.Script(fake.Call("slow", nil), fake.Text("ok")), Tools: tool.NewRegistry(slow)}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil || !strings.Contains(res.Messages[2].Content, "deadline") {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestCallNotOfferedIsDenied(t *testing.T) {
	ran := false
	secret := tool.Func(tool.Spec{Name: "secret"}, func(context.Context, json.RawMessage) (string, error) {
		ran = true
		return "", nil
	})
	a := &harness.Agent{Provider: fake.Script(fake.Call("secret", nil), fake.Text("ok")), Tools: tool.NewRegistry(secret),
		Hooks: []harness.Hook{hideTools{}}}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil || ran || !strings.Contains(res.Messages[2].Content, "not offered") {
		t.Fatalf("%v ran=%v %+v", err, ran, res)
	}
}

type hideTools struct{}

func (hideTools) Name() string { return "hide" }
func (hideTools) BeforeModel(_ context.Context, _ harness.Run, req *model.Request) error {
	req.Tools = nil
	return nil
}

func TestCancellationIsRecorded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stopper := tool.Func(tool.Spec{Name: "stop"}, func(context.Context, json.RawMessage) (string, error) {
		cancel()
		return "ok", nil
	})
	a := &harness.Agent{Provider: fake.Script(fake.Call("stop", nil), fake.Text("no")), Tools: tool.NewRegistry(stopper)}
	res, err := a.Run(ctx, harness.Prompt("x"))
	if err != nil || res.Stop != harness.StopCancelled || res.State != sp {
		t.Fatalf("%v %+v", err, res)
	}
	balanced(t, res.Messages)
}

// compactor is a reference hook: the summariser's nondeterminism is recorded.
type compactor struct{ runs *int }

func (compactor) Name() string { return "compact" }
func (c compactor) Context(_ context.Context, r harness.Run) (harness.Edit, error) {
	if len(r.Messages) < 3 {
		return harness.Edit{}, nil
	}
	*c.runs++
	return harness.Edit{To: len(r.Messages), Source: "compact",
		With: []model.Message{model.User(fmt.Sprintf("summary #%d", *c.runs))}}, nil
}

func TestDurableEditIsRecordedWithProvenance(t *testing.T) {
	j := harness.NewMemJournal()
	runs := 0
	a := &harness.Agent{Journal: j, Provider: fake.Script(fake.Call("echo", nil), fake.Text("ok")),
		Tools: tool.NewRegistry(echoTool()), Hooks: []harness.Hook{compactor{&runs}}}
	in := harness.Prompt("x")
	in.RunID = "r"
	res, err := a.Run(context.Background(), in)
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	r, _ := harness.Fold(mustLoad(t, j, "r"))
	if r.Messages[0].Source != "compact" || r.Messages[0].Content != "summary #1" || runs != 1 {
		t.Fatalf("%+v runs=%d", r.Messages, runs)
	}
}

type badEdit struct{}

func (badEdit) Name() string { return "bad" }
func (badEdit) Context(_ context.Context, r harness.Run) (harness.Edit, error) {
	if len(r.Messages) == 3 {
		return harness.Edit{To: 2, With: []model.Message{model.User("oops")}}, nil // orphans the result
	}
	return harness.Edit{}, nil
}

func TestEditThatBreaksPairingFailsTheRun(t *testing.T) {
	a := &harness.Agent{Provider: fake.Script(fake.Call("echo", nil), fake.Text("ok")), Tools: tool.NewRegistry(echoTool()),
		Hooks: []harness.Hook{badEdit{}}}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err == nil || res.State != fl || res.Class != model.ClassInternal {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestHookCannotChangeTheRun(t *testing.T) {
	a := &harness.Agent{Provider: fake.Script(fake.Text("ok")), Hooks: []harness.Hook{vandal{}}}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil || res.Messages[0].Content != "x" {
		t.Fatalf("%v %+v", err, res)
	}
}

type vandal struct{}

func (vandal) Name() string { return "vandal" }
func (vandal) BeforeModel(_ context.Context, r harness.Run, _ *model.Request) error {
	r.Messages[0].Content = "rewritten"
	r.Turn = 99
	return nil
}

func TestTableOnlyHoldsReachableKindsAndRefusesTheRest(t *testing.T) {
	if harness.CanTransition(co, harness.KindToolDone, to) {
		t.Error("compose cannot take tool.done")
	}
	_, err := harness.Fold([]harness.Event{
		{Seq: 1, Kind: harness.KindRunStarted, Pin: &harness.Pin{Schema: 1}},
		{Seq: 2, Kind: harness.KindModelDone, Reply: &model.Response{Text: "x"}},
	})
	if !errors.Is(err, harness.ErrIllegalTransition) {
		t.Fatalf("a journal with an illegal walk folded: %v", err)
	}
}

func TestNoteLandsOnTheHookEvent(t *testing.T) {
	var text string
	ctx := harness.WithEvents(context.Background(), func(e harness.Event) {
		if e.Kind == harness.KindHook && e.Hook == "noter" {
			text = e.Text
		}
	})
	a := &harness.Agent{Provider: fake.Script(fake.Text("ok")), Hooks: []harness.Hook{noter{}}}
	if _, err := a.Run(ctx, harness.Prompt("hi")); err != nil {
		t.Fatal(err)
	}
	if text != "saw 1 messages" {
		t.Errorf("note = %q", text)
	}
}

type noter struct{}

func (noter) Name() string { return "noter" }
func (noter) BeforeModel(ctx context.Context, r harness.Run, _ *model.Request) error {
	harness.Note(ctx, "saw %d messages", len(r.Messages))
	return nil
}
