package trace_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/trace"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func subagent(child *harness.Agent) tool.Tool {
	return tool.Func(tool.Spec{Name: "delegate", Effect: tool.EffectIdempotent}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		in := harness.Prompt("sub")
		in.RunID = tool.IdempotencyKey(ctx)
		res, err := child.Run(ctx, in)
		if err != nil {
			return "", err
		}
		return res.Output, nil
	})
}

func TestSpansNestAcrossSubAgents(t *testing.T) {
	mem := &trace.Memory{}
	ctx := harness.WithEvents(context.Background(), trace.Sink(mem))
	child := &harness.Agent{Name: "child", Provider: fake.Script(fake.Text("hi"))}
	lead := &harness.Agent{Name: "lead", Provider: fake.Script(fake.Call("delegate", nil), fake.Text("done")),
		Tools: tool.NewRegistry(subagent(child))}
	if _, err := lead.Run(ctx, harness.Prompt("x")); err != nil {
		t.Fatal(err)
	}
	var leadSpan, childSpan trace.SpanData
	for _, s := range mem.Spans() {
		switch s.Name {
		case "invoke_agent lead":
			leadSpan = s
		case "invoke_agent child":
			childSpan = s
		}
	}
	if leadSpan.SpanID == "" || childSpan.ParentID != leadSpan.SpanID || childSpan.TraceID != leadSpan.TraceID {
		t.Fatalf("lead=%+v child=%+v", leadSpan, childSpan)
	}
}

type dying struct {
	harness.Journal
	at, n int
}

func (d *dying) Append(ctx context.Context, id string, evs []harness.Event) error {
	if d.n++; d.n == d.at {
		return errors.New("process died")
	}
	return d.Journal.Append(ctx, id, evs)
}

// The same step gets the same identity from whichever process reports it.
func TestSpanIdentitySurvivesAResume(t *testing.T) {
	run := func(j harness.Journal, mem *trace.Memory) {
		ctx := harness.WithEvents(context.Background(), trace.Sink(mem))
		noop := tool.Func(tool.Spec{Name: "noop", Effect: tool.EffectIdempotent}, func(context.Context, json.RawMessage) (string, error) { return "x", nil })
		p := fake.NewFunc(func(req model.Request) (model.Response, error) {
			if req.Messages[len(req.Messages)-1].Role == model.RoleTool {
				return fake.Text("done"), nil
			}
			return fake.Call("noop", nil), nil
		})
		a := &harness.Agent{Name: "a", Journal: j, Provider: p, Tools: tool.NewRegistry(noop)}
		in := harness.Prompt("x")
		in.RunID = "r"
		_, _ = a.Run(ctx, in)
	}
	whole, broken, again := &trace.Memory{}, &trace.Memory{}, &trace.Memory{}
	run(harness.NewMemJournal(), whole)
	j := harness.NewMemJournal()
	run(&dying{Journal: j, at: 6}, broken) // before tool.done
	run(j, again)
	want := map[string]bool{}
	for _, s := range whole.Spans() {
		want[s.SpanID] = true
	}
	for _, s := range append(broken.Spans(), again.Spans()...) {
		if !want[s.SpanID] {
			t.Errorf("span %s has an identity the uninterrupted run did not: %+v", s.Name, s)
		}
	}
}
