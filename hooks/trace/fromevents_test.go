package trace_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/trace"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

// ask parks on its first call and answers on the second, once the signal is in.
func ask() tool.Tool {
	return tool.Func(tool.Spec{Name: "ask", Effect: tool.EffectIdempotent}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		if len(harness.Signals(ctx)) > 0 {
			return "tool-result-secret", nil
		}
		return "", harness.Suspend(harness.Wait{ID: "w1", Kind: "user"})
	})
}

// record runs a model call, a tool that parks and resumes, and a final answer,
// observed live by a sink and recorded in a journal.
func record(t *testing.T) (live []trace.SpanData, events []harness.Event) {
	t.Helper()
	mem := &trace.Memory{}
	j := harness.NewMemJournal()
	ctx := harness.WithEvents(context.Background(), trace.Sink(mem))
	a := &harness.Agent{Name: "lead", Model: "m1", Journal: j,
		Provider: fake.Script(
			model.Response{ToolCalls: []model.ToolCall{{ID: "c1", Name: "ask", Arguments: json.RawMessage(`{"q":"secret-arg"}`)}}, Usage: model.Usage{Input: 5, Output: 2}},
			model.Response{Text: "final-secret-answer", Usage: model.Usage{Input: 9, Output: 4}}),
		Tools: tool.NewRegistry(ask())}
	in := harness.Prompt("x")
	in.RunID = "r"
	res, err := a.Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	if res, err = a.Run(ctx, harness.Resume("r", harness.Signal{Wait: "w1", Value: "typed-secret"})); err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	events, err = j.Load(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	return mem.Spans(), events
}

func TestFromEventsEqualsTheLiveSink(t *testing.T) {
	live, events := record(t)
	got := trace.FromEvents(events)
	if !reflect.DeepEqual(got, live) {
		t.Fatalf("FromEvents differs from the live sink\n got: %+v\nlive: %+v", got, live)
	}
	var names []string
	for _, s := range got {
		names = append(names, s.Name)
	}
	want := "[chat execute_tool ask wait user execute_tool ask chat invoke_agent lead]"
	if fmt.Sprint(names) != want {
		t.Fatalf("spans = %v, want %s", names, want)
	}
}

func TestWaitSpansCarryKindAndToolSpansTheirOutcome(t *testing.T) {
	_, events := record(t)
	var wait, parked, finished *trace.SpanData
	spans := trace.FromEvents(events)
	for i, s := range spans {
		switch {
		case s.Name == "wait user":
			wait = &spans[i]
		case s.Name == "execute_tool ask" && parked == nil:
			parked = &spans[i]
		case s.Name == "execute_tool ask":
			finished = &spans[i]
		}
	}
	if wait == nil || parked == nil || finished == nil {
		t.Fatalf("%+v", spans)
	}
	if wait.Attrs["harness.wait.kind"] != "user" || wait.Attrs["harness.wait.expired"] != false || !wait.End.After(wait.Start) {
		t.Errorf("wait span: %+v", wait)
	}
	if parked.Attrs["harness.tool.outcome"] != "parked" || finished.Attrs["harness.tool.outcome"] != "ok" {
		t.Errorf("tool outcomes: %v %v", parked.Attrs, finished.Attrs)
	}
}
