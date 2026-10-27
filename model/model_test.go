package model_test

import (
	"errors"
	"testing"

	"github.com/ecociel/agent-harness/model"
)

func TestCollectMergesDeltas(t *testing.T) {
	seq := func(yield func(model.Event, error) bool) {
		_ = yield(model.TextDelta{Text: "hel"}, nil) &&
			yield(model.TextDelta{Text: "lo"}, nil) &&
			yield(model.ToolCallDelta{Index: 1, Name: "b", Arguments: `{"x":`}, nil) &&
			yield(model.ToolCallDelta{Index: 0, ID: "a1", Name: "a"}, nil) &&
			yield(model.ToolCallDelta{Index: 1, Arguments: `1}`}, nil) &&
			yield(model.Finish{Reason: model.FinishToolCalls, Usage: model.Usage{Input: 3, Output: 2}}, nil)
	}
	var n int
	resp, err := model.Collect(seq, func(model.Event) { n++ })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "hello" || n != 6 {
		t.Fatalf("text=%q events=%d", resp.Text, n)
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].ID != "a1" || string(resp.ToolCalls[0].Arguments) != "{}" {
		t.Fatalf("calls[0] = %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[1].ID != "call_1" || string(resp.ToolCalls[1].Arguments) != `{"x":1}` {
		t.Fatalf("calls[1] = %+v", resp.ToolCalls[1])
	}
	if resp.Usage.Total() != 5 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestCollectStopsOnError(t *testing.T) {
	boom := errors.New("boom")
	if _, err := model.Collect(model.Fail(boom), nil); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamRoundTrip(t *testing.T) {
	in := model.Response{Text: "a b  c", ToolCalls: []model.ToolCall{{ID: "1", Name: "t", Arguments: []byte(`{"k":"v"}`)}}}
	out, err := model.Collect(model.Stream(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != in.Text || len(out.ToolCalls) != 1 || out.Finish != model.FinishToolCalls {
		t.Fatalf("got %+v", out)
	}
}
