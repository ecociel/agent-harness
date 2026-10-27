package harness_test

import (
	"context"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
)

type noter struct{}

func (noter) Name() string { return "noter" }

func (noter) BeforeModel(_ context.Context, r *harness.Run, _ *model.Request) error {
	r.Note("noter", "saw %d messages", len(r.Messages))
	return nil
}

func (noter) AfterModel(context.Context, *harness.Run, *model.Response) error { return nil }

func TestNoteLandsOnTheHookEvent(t *testing.T) {
	var texts = map[harness.Point]string{}
	ctx := harness.WithEvents(context.Background(), func(e harness.Event) {
		if e.Kind == harness.KindHook && e.Hook == "noter" {
			texts[e.Point] = e.Text
		}
	})
	a := &harness.Agent{Provider: fake.Script(fake.Text("ok")), Hooks: []harness.Hook{noter{}}}
	if _, err := a.Run(ctx, harness.Prompt("hi")); err != nil {
		t.Fatal(err)
	}
	if got := texts[harness.PointBeforeModel]; got != "saw 1 messages" {
		t.Errorf("BeforeModel text = %q", got)
	}
	// The note is consumed by the invocation that set it.
	if got, ok := texts[harness.PointAfterModel]; !ok || got != "" {
		t.Errorf("AfterModel text = %q (present %v), want empty", got, ok)
	}
}
