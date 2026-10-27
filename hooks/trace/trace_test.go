package trace_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/trace"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func TestSpansNestAcrossSubAgents(t *testing.T) {
	mem := &trace.Memory{}
	ctx := trace.WithTracer(context.Background(), trace.NewTracer(mem))
	hook := &trace.Hook{} // uses the tracer on ctx

	child := &harness.Agent{Name: "child", Model: "m", Provider: fake.Script(fake.Text("leaf")), Hooks: []harness.Hook{hook}}
	ask := tool.Func(tool.Spec{Name: "ask"}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		r, err := child.Run(ctx, harness.Prompt("q"))
		if err != nil {
			return "", err
		}
		return r.Output, nil
	})
	parent := &harness.Agent{Name: "parent", Model: "m", Provider: fake.Script(fake.Call("ask", nil), fake.Text("root")),
		Tools: tool.NewRegistry(ask), Hooks: []harness.Hook{hook}}
	if _, err := parent.Run(ctx, harness.Prompt("go")); err != nil {
		t.Fatal(err)
	}
	byName := map[string]trace.SpanData{}
	for _, s := range mem.Spans() {
		byName[s.Name] = s
	}
	root, tl, sub := byName["invoke_agent parent"], byName["execute_tool ask"], byName["invoke_agent child"]
	if root.SpanID == "" || tl.ParentID != root.SpanID || sub.ParentID != tl.SpanID || sub.TraceID != root.TraceID {
		t.Fatalf("bad tree: root=%+v tool=%+v sub=%+v", root, tl, sub)
	}
	if byName["chat m"].Attrs["gen_ai.usage.input_tokens"] == nil {
		t.Fatal("model span lacks usage")
	}
	if len(root.Events) == 0 {
		t.Fatal("no state events on run span")
	}
}

func TestStartWithoutTracerIsNoop(t *testing.T) {
	ctx, s := trace.Start(context.Background(), "x", nil)
	s.SetAttr("a", 1)
	s.Finish(nil)
	if trace.SpanFrom(ctx) != nil {
		t.Fatal("span leaked")
	}
}
