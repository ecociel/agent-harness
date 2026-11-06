package model_test

import (
	"encoding/json"
	"errors"
	"strings"
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

func TestPartBytesNeverReachJSON(t *testing.T) {
	m := model.Message{Role: model.RoleUser, Content: "see attached", Parts: []model.Part{{
		Kind: model.PartImage, MediaType: "image/png", Name: "scan.png", Ref: "sha256:ab12", Text: "a scanned invoice", Bytes: []byte("PNGDATA")}}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"role":"user","content":"see attached","parts":[{"kind":"image","media_type":"image/png","name":"scan.png","ref":"sha256:ab12","text":"a scanned invoice"}]}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
}

func TestCloneCopiesParts(t *testing.T) {
	orig := model.Request{Messages: []model.Message{{Role: model.RoleUser, Parts: []model.Part{{Kind: model.PartImage, Ref: "a"}}}}}
	c := orig.Clone()
	c.Messages[0].Parts[0].Bytes = []byte("x")
	c.Messages[0].Parts = append(c.Messages[0].Parts, model.Part{Ref: "b"})
	if orig.Messages[0].Parts[0].Bytes != nil || len(orig.Messages[0].Parts) != 1 {
		t.Fatalf("clone wrote through: %+v", orig.Messages[0].Parts)
	}
}

func TestEstimateTokensCountsPartText(t *testing.T) {
	msgs := []model.Message{{Role: model.RoleUser, Parts: []model.Part{
		{Kind: model.PartDocument, Ref: "a", Text: strings.Repeat("x", 400)},
		{Kind: model.PartImage, Ref: "b"},
	}}}
	if got := model.EstimateTokens(msgs); got != 104 {
		t.Fatalf("got %d, want 104", got)
	}
}
