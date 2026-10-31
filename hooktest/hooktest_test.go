package hooktest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/approval"
	"github.com/ecociel/agent-harness/hooks/budget"
	"github.com/ecociel/agent-harness/hooks/compact"
	"github.com/ecociel/agent-harness/hooks/guard"
	"github.com/ecociel/agent-harness/hooks/offload"
	"github.com/ecociel/agent-harness/hooks/profile"
	"github.com/ecociel/agent-harness/hooks/retry"
	"github.com/ecociel/agent-harness/hooks/trajectory"
	"github.com/ecociel/agent-harness/hooktest"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

var look = tool.Func(tool.Spec{Name: "look", Effect: tool.EffectRead}, func(context.Context, json.RawMessage) (string, error) {
	return strings.Repeat("seen ", 50), nil
})

func script() model.Provider {
	return fake.Script(fake.Call("look", nil), fake.Call("look", nil), fake.Text("done"))
}

// Every built-in hook, in one run: none may write through what it was given.
func TestTheBuiltInHooksLeaveTheirRunAlone(t *testing.T) {
	store := offload.NewMemStore()
	a := &harness.Agent{Name: "a", Model: "m", Provider: script(), Tools: tool.NewRegistry(look, offload.Reader(store)),
		Hooks: hooktest.Frozen(t, &guard.Guard{OutputDeny: guard.Secrets()}, guard.Redactor(guard.PII()),
			budget.New(budget.Limits{Tokens: 1 << 20, RepeatCalls: 9}), &approval.Policy{},
			&compact.Compactor{MaxTokens: 50, KeepLast: 2}, offload.New(store, 100),
			profile.Profiles{{Match: "m", Instructions: "be brief"}}, &retry.Retry{Base: time.Millisecond}, trajectory.New())}
	res, err := a.Run(context.Background(), harness.Prompt("hello"))
	if err != nil || res.Output != "done" {
		t.Fatalf("%v %+v", err, res)
	}
}

// fails records what a test would have failed on.
type fails struct {
	testing.TB
	msg []string
}

func (f *fails) Errorf(format string, a ...any) { f.msg = append(f.msg, fmt.Sprintf(format, a...)) }

// writer writes through a slice the machine shares, at one point.
type writer struct{ at harness.Point }

func (writer) Name() string { return "writer" }

func (w writer) write(p harness.Point, r harness.Run) {
	if p != w.at {
		return
	}
	for i := range r.Messages {
		for j := range r.Messages[i].ToolCalls {
			r.Messages[i].ToolCalls[j].Name = "changed"
		}
	}
	r.Events[0].Text = "changed"
}

func (w writer) BeforeModel(_ context.Context, r harness.Run, _ *model.Request) error {
	w.write(harness.PointBeforeModel, r)
	return nil
}
func (w writer) BeforeTool(_ context.Context, r harness.Run, _ harness.Call) harness.Decision {
	w.write(harness.PointBeforeTool, r)
	return harness.Allow()
}
func (w writer) WrapModel(r harness.Run, next model.Provider) model.Provider {
	return model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
		w.write(harness.PointWrapModel, r)
		return next.Complete(ctx, req)
	})
}
func (w writer) WrapTool(r harness.Run, next harness.ToolFunc) harness.ToolFunc {
	return func(ctx context.Context, c harness.Call) (string, error) {
		w.write(harness.PointWrapTool, r)
		return next(ctx, c)
	}
}
func (w writer) AfterTool(_ context.Context, r harness.Run, _ harness.Call, s string) (string, error) {
	w.write(harness.PointAfterTool, r)
	return s, nil
}
func (w writer) AfterRun(_ context.Context, r harness.Run) { w.write(harness.PointAfterRun, r) }

func TestAHookThatWritesThroughItsRunFails(t *testing.T) {
	for _, at := range []harness.Point{harness.PointBeforeModel, harness.PointWrapModel, harness.PointBeforeTool,
		harness.PointWrapTool, harness.PointAfterTool, harness.PointAfterRun} {
		t.Run(string(at), func(t *testing.T) {
			f := &fails{TB: t}
			a := &harness.Agent{Provider: script(), Tools: tool.NewRegistry(look), Hooks: hooktest.Frozen(f, writer{at})}
			if _, err := a.Run(context.Background(), harness.Prompt("x")); err != nil {
				t.Fatal(err)
			}
			if len(f.msg) == 0 || !strings.Contains(f.msg[0], string(at)) {
				t.Fatalf("not caught: %q", f.msg)
			}
		})
	}
}

// A frozen hook must decide what it would have decided.
func TestFrozenHooksStillDecide(t *testing.T) {
	ran := 0
	counted := tool.Func(tool.Spec{Name: "look"}, func(context.Context, json.RawMessage) (string, error) {
		ran++
		return "seen", nil
	})
	a := &harness.Agent{Provider: fake.Script(fake.Call("look", nil), fake.Text("refused")), Tools: tool.NewRegistry(counted),
		Hooks: hooktest.Frozen(t, &guard.Guard{ToolDeny: []string{"look"}})}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil || ran != 0 || res.Output != "refused" {
		t.Fatalf("%v %+v, ran %d", err, res, ran)
	}
}
