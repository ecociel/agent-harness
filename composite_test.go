package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/tool"
)

// A body that parks twice hears only the answer to its latest ask. An older answer
// would reach a child that has moved on, and the child would refuse the delivery that
// holds the new one.
func TestABodyHearsOnlyTheAnswersToItsLatestPark(t *testing.T) {
	ctx := context.Background()
	j := newJSONJournal()
	var heard [][]string
	asks := 0
	c := &harness.Composite{Name: "c", Shape: "test", Journal: j,
		Body: func(_ context.Context, in harness.Input) (string, harness.Spend, error) {
			var values []string
			for _, s := range in.Signals {
				values = append(values, s.Value)
			}
			heard = append(heard, values)
			if asks < 2 {
				asks++
				return "", harness.Spend{}, harness.Suspend(harness.Wait{ID: "w" + string(rune('0'+asks)), Run: "child", Kind: "approval"})
			}
			return "done", harness.Spend{}, nil
		}}
	in := harness.Prompt("x")
	in.RunID = "r"
	res, err := c.Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	res, err = c.Run(ctx, harness.Resume("r", harness.Signal{Wait: res.Waits[0].ID, Value: "first"}))
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	res, err = c.Run(ctx, harness.Resume("r", harness.Signal{Wait: res.Waits[0].ID, Value: "second"}))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	want := [][]string{nil, {"first"}, {"second"}}
	if len(heard) != 3 || !slices.Equal(heard[0], want[0]) || !slices.Equal(heard[1], want[1]) || !slices.Equal(heard[2], want[2]) {
		t.Fatalf("heard %v, want %v", heard, want)
	}
}

// Nothing a body says outside the journal is true, so a crash after its record and
// before anything else must not repeat its work.
func TestARecordedBodyDoesNotRunAgain(t *testing.T) {
	ctx := context.Background()
	j := newJSONJournal()
	runs := 0
	c := func(jj harness.Journal) *harness.Composite {
		return &harness.Composite{Name: "c", Shape: "test", Journal: jj,
			Body: func(context.Context, harness.Input) (string, harness.Spend, error) {
				runs++
				return "done", harness.Spend{ModelCalls: 7}, nil
			}}
	}
	in := harness.Prompt("x")
	in.RunID = "r"
	// run.started, then body.done lands and the worker never hears back.
	if _, err := c(&crashJournal{Journal: j, at: 2, after: true}).Run(ctx, in); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	res, err := c(j).Run(ctx, harness.Resume("r"))
	if err != nil || !res.OK() || res.Output != "done" {
		t.Fatalf("%v %+v", err, res)
	}
	if runs != 1 {
		t.Fatalf("the body ran %d times", runs)
	}
	if res.ModelCalls != 0 || res.Spent().ModelCalls != 7 {
		t.Fatalf("own %d, reported %d", res.ModelCalls, res.Spent().ModelCalls)
	}
}

// A body that fails ends the run, keeps its error, and is not retried.
func TestAFailedBodyFailsTheRun(t *testing.T) {
	boom := errors.New("boom")
	c := &harness.Composite{Name: "c", Shape: "test", Journal: newJSONJournal(),
		Body: func(context.Context, harness.Input) (string, harness.Spend, error) { return "", harness.Spend{}, boom }}
	in := harness.Prompt("x")
	in.RunID = "r"
	res, err := c.Run(context.Background(), in)
	if !errors.Is(err, boom) || res == nil || res.State != harness.StateFailed {
		t.Fatalf("%v %+v", err, res)
	}
}

// One ID, two kinds of worker: neither may take the other's journal. A composite has
// no provider, and an agent would never run the body.
func TestARunRefusesAWorkerOfAnotherShape(t *testing.T) {
	ctx := context.Background()
	w := newWorld()
	body := func(context.Context, harness.Input) (string, harness.Spend, error) {
		return "done", harness.Spend{}, nil
	}

	j := newJSONJournal()
	in := harness.Prompt("x")
	in.RunID = "r"
	if _, err := (&harness.Agent{Journal: &crashJournal{Journal: j, at: 2}, Provider: w.provider("pay")}).Run(ctx, in); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	res, err := (&harness.Composite{Name: "c", Shape: "test", Journal: j, Body: body}).Run(ctx, harness.Resume("r"))
	if err == nil || res == nil || res.State != harness.StateFailed || !strings.Contains(res.Detail, "changed") {
		t.Fatalf("a composite took an agent's run: %v %+v", err, res)
	}

	j = newJSONJournal()
	if _, err := (&harness.Composite{Name: "c", Shape: "test", Journal: &crashJournal{Journal: j, at: 2}, Body: body}).Run(ctx, in); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	res, err = (&harness.Agent{Journal: j, Provider: w.provider("pay")}).Run(ctx, harness.Resume("r"))
	if err == nil || res == nil || res.State != harness.StateFailed || !strings.Contains(res.Detail, "changed") {
		t.Fatalf("an agent took a composite's run: %v %+v", err, res)
	}
}

// Cancel is optional: a composite with nothing to walk still ends.
func TestACompositeWithNoCancelStillEnds(t *testing.T) {
	ctx := context.Background()
	c := &harness.Composite{Name: "c", Shape: "test", Journal: newJSONJournal(),
		Body: func(context.Context, harness.Input) (string, harness.Spend, error) {
			return "", harness.Spend{}, harness.Suspend(harness.Wait{ID: "w", Run: "child", Kind: "approval"})
		}}
	in := harness.Prompt("x")
	in.RunID = "r"
	if res, err := c.Run(ctx, in); err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	res, err := c.Run(ctx, harness.Input{RunID: "r", Cancel: "stop now"})
	if err != nil || res.Stop != harness.StopCancelled || !res.State.Terminal() {
		t.Fatalf("%v %+v", err, res)
	}
}

// A lost call to a tool that has since changed cannot be decided again, and alone in
// its batch it empties the batch. The run goes back to the model, which hears why, and
// the worker does not wedge.
func TestALostCallToAChangedToolGoesBackToTheModel(t *testing.T) {
	ctx := context.Background()
	j := newJSONJournal()
	w := newWorld()
	build := func(jj harness.Journal, description string) *harness.Agent {
		pay := tool.Func(tool.Spec{Name: "pay", Description: description, Effect: tool.EffectIdempotent},
			func(context.Context, json.RawMessage) (string, error) { return "paid", nil })
		return &harness.Agent{Name: "a", Journal: jj, Provider: w.provider("pay"), Tools: tool.NewRegistry(pay)}
	}
	in := harness.Prompt("x")
	in.RunID = "r"
	// run.started, model.started, model.done, tool.gated, then tool.started lands.
	if _, err := build(&crashJournal{Journal: j, at: 5, after: true}, "old").Run(ctx, in); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	res, err := build(j, "new").Run(ctx, harness.Resume("r"))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if !slices.ContainsFunc(res.Messages, func(m model.Message) bool {
		return m.Role == model.RoleTool && strings.Contains(m.Content, `"denied":true`)
	}) {
		t.Fatalf("the model was not told: %+v", res.Messages)
	}
}
