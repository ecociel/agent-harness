package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/tool"
)

// admissions counts the workers that admitted the run.
type admissions struct{ n *int }

func (admissions) Name() string                                   { return "admissions" }
func (a admissions) BeforeRun(context.Context, harness.Run) error { *a.n++; return nil }

// scrubAlice keeps a signature out of everything.
type scrubAlice struct{}

func (scrubAlice) Name() string { return "scrub" }
func (scrubAlice) Redact(_ context.Context, _ harness.Kind, s string) (string, error) {
	return strings.ReplaceAll(s, " -- alice", ""), nil
}

// A handler answers a run with Deliver, and a worker in another process does the work.
func TestADecisionDeliveredByOneProcessIsSeenByAWorkerInAnother(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	w := newWorld()
	worker := approvalAgent(w, j, 0)
	in := harness.Prompt("send it")
	in.RunID = "d"
	res, err := worker.Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	before := len(mustLoad(t, j, "d"))
	paid := w.paid

	admitted := 0
	handler := approvalAgent(w, j, 0)
	handler.Hooks = append(handler.Hooks, admissions{&admitted}, scrubAlice{})
	at := time.Now()
	sig := harness.Signal{Wait: res.Waits[0].ID, Value: "yes -- alice", By: "alice"}
	got, err := handler.Deliver(ctx, harness.Resume("d", sig))
	if err != nil || got.State != harness.StateGate {
		t.Fatalf("deliver: %v %+v", err, got)
	}
	evs := mustLoad(t, j, "d")[before:]
	if len(evs) != 1 || evs[0].Kind != harness.KindWaitResolved {
		t.Fatalf("Deliver wrote %+v", evs)
	}
	if evs[0].Signal.Value != "yes" {
		t.Fatalf("the redactor did not see the signal: %q", evs[0].Signal.Value)
	}
	if wake := evs[0].Wake; wake.Before(at) || wake.After(time.Now()) {
		t.Fatalf("Wake %v is not now", wake)
	}
	if admitted != 0 || w.paid != paid || len(w.effect["send"]) != 0 {
		t.Fatalf("Deliver worked on the run: admitted %d, paid %d, sent %d", admitted, w.paid-paid, len(w.effect["send"]))
	}

	if _, err := handler.Deliver(ctx, harness.Resume("d", sig)); err != nil {
		t.Fatalf("a repeat: %v", err)
	}
	if n := len(mustLoad(t, j, "d")); n != before+1 {
		t.Fatalf("a repeat wrote %d events", n-before-1)
	}
	if res, err = approvalAgent(w, j, 0).Run(ctx, harness.Resume("d")); err != nil || !res.OK() {
		t.Fatalf("worker: %v %+v", err, res)
	}
	if n := len(w.effect["send"]); n != 1 {
		t.Fatalf("send ran %d times", n)
	}
}

func TestDeliverRefusesWhatNoWaitAsked(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	w := newWorld()
	if _, err := approvalAgent(w, j, 0).Deliver(ctx, harness.Resume("missing", harness.Signal{Wait: "w"})); !errors.Is(err, harness.ErrNoSuchRun) {
		t.Fatalf("an unknown run: %v", err)
	}
	in := harness.Prompt("send it")
	in.RunID = "stale"
	if _, err := approvalAgent(w, j, time.Millisecond).Run(ctx, in); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	before := len(mustLoad(t, j, "stale"))
	res, _ := harness.Fold(mustLoad(t, j, "stale"))
	for _, sig := range []harness.Signal{{Wait: "nobody asked"}, {Wait: res.Waits[0].ID, Value: "yes"}} {
		if _, err := approvalAgent(w, j, 0).Deliver(ctx, harness.Resume("stale", sig)); !errors.Is(err, harness.ErrStaleSignal) {
			t.Fatalf("signal %q: want ErrStaleSignal, got %v", sig.Wait, err)
		}
	}
	if n := len(mustLoad(t, j, "stale")); n != before {
		t.Fatalf("a refused delivery wrote %d events", n-before)
	}
}

// A cancel delivered while a worker is inside a tool takes the next position, so
// the worker cannot record anything after it.
func TestACancelDeliveredFencesTheWorker(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	w := newWorld()
	entered, release := make(chan struct{}), make(chan struct{})
	slow := tool.Func(tool.Spec{Name: "slow", Effect: tool.EffectIdempotent}, func(context.Context, json.RawMessage) (string, error) {
		close(entered)
		<-release
		return "late", nil
	})
	a := &harness.Agent{Journal: j, Provider: w.provider("slow"), Tools: tool.NewRegistry(slow)}
	in := harness.Prompt("go")
	in.RunID = "cancel"
	done := make(chan error, 1)
	go func() { _, err := a.Run(ctx, in); done <- err }()
	<-entered
	res, err := a.Deliver(ctx, harness.Input{RunID: "cancel", Cancel: "stop"})
	if err != nil || res.State != harness.StateClosing {
		t.Fatalf("deliver: %v %+v", err, res)
	}
	close(release)
	if err := <-done; !errors.Is(err, harness.ErrFenced) {
		t.Fatalf("the worker must be fenced, got %v", err)
	}
	if res, err = a.Run(ctx, harness.Resume("cancel")); err != nil || res.Stop != harness.StopCancelled || !res.State.Terminal() {
		t.Fatalf("%v %+v", err, res)
	}
}
