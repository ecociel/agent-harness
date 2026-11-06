package trace_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
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
func record(t *testing.T, opts ...trace.Option) (live []trace.SpanData, events []harness.Event) {
	t.Helper()
	mem := &trace.Memory{}
	j := harness.NewMemJournal()
	ctx := harness.WithEvents(context.Background(), trace.SinkWith(opts, mem))
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
	for name, opts := range map[string][]trace.Option{
		"content none": nil,
		"content full": {trace.WithContent(trace.ContentFull)},
	} {
		t.Run(name, func(t *testing.T) {
			live, events := record(t, opts...)
			got := trace.FromEvents(events, opts...)
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
		})
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

func dump(spans []trace.SpanData) string {
	b, _ := json.Marshal(spans)
	return string(b)
}

func TestContentStaysOutOfSpansUnlessAsked(t *testing.T) {
	secrets := []string{"secret-arg", "tool-result-secret", "final-secret-answer", "typed-secret"}
	_, events := record(t)
	none := dump(trace.FromEvents(events))
	for _, s := range secrets {
		if contains(none, s) {
			t.Errorf("ContentNone leaked %q: %s", s, none)
		}
	}
	full := trace.FromEvents(events, trace.WithContent(trace.ContentFull))
	var args, result, reply, value bool
	for _, s := range full {
		args = args || s.Attrs["gen_ai.tool.call.arguments"] == `{"q":"secret-arg"}`
		result = result || s.Attrs["gen_ai.tool.call.result"] == "tool-result-secret"
		reply = reply || s.Attrs["gen_ai.output.text"] == "final-secret-answer"
		value = value || s.Attrs["harness.wait.value"] == "typed-secret"
	}
	if !args || !result || !reply || !value {
		t.Errorf("ContentFull missing args=%v result=%v reply=%v value=%v: %s", args, result, reply, value, dump(full))
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestAFailedRunsErrorTextIsContent(t *testing.T) {
	ev := []harness.Event{{Seq: 3, Kind: harness.KindRunEnded, RunID: "r", Agent: "a", To: harness.StateFailed, Outcome: "error", Text: "boom: customer@example.com"}}
	if got := trace.FromEvents(ev)[0]; got.Status != "error" || got.Error != "error" {
		t.Errorf("%+v", got)
	}
	if got := trace.FromEvents(ev, trace.WithContent(trace.ContentFull))[0]; got.Error != "boom: customer@example.com" {
		t.Errorf("%+v", got)
	}
}
