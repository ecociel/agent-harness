package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/tool"
)

type stopMidStream struct{}

func (stopMidStream) Name() string { return "stop" }
func (stopMidStream) WrapModel(_ harness.Run, _ model.Provider) model.Provider {
	return model.ProviderFunc(func(context.Context, model.Request) iter.Seq2[model.Event, error] {
		return model.Fail(harness.Stop(harness.StopGuardrail, "blocked mid stream"))
	})
}

// The documented streaming guardrail: WrapModel ends the stream with Stop.
func TestStreamingGuardrailStopEndsTheRun(t *testing.T) {
	w := newWorld()
	a := &harness.Agent{Provider: w.provider(), Hooks: []harness.Hook{stopMidStream{}}}
	res, err := a.Run(context.Background(), harness.Prompt("go"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != harness.StopGuardrail || res.State != harness.StateStopped {
		t.Fatalf("%s %s", res.Stop, res.State)
	}
}

// An unknown outcome inside a child must be answered inside the child. The child
// holds the reply the lead is waiting for.
func TestUnknownOutcomeInAChildIsAnsweredInTheChild(t *testing.T) {
	ctx := context.Background()
	w := newWorld()
	inner := newJSONJournal()
	child := &harness.Agent{Name: "child", Provider: w.provider("pay"), Tools: tool.NewRegistry(w.tool("pay", tool.EffectOnce))}
	build := func(j harness.Journal) *harness.Agent {
		return &harness.Agent{Name: "lead", Journal: j, Provider: w.provider("delegate"),
			Tools: tool.NewRegistry(subagentTool(child, "delegate"))}
	}
	in := harness.Prompt("go")
	in.RunID = "lead-run"
	// Lead: started, model x2, gated, started (5). Child: started, model x2, gated, started (10).
	if _, err := build(&crashJournal{Journal: inner, at: 10, after: true}).Run(ctx, in); !errors.Is(err, errCrash) {
		t.Fatalf("want crash, got %v", err)
	}
	res, err := build(inner).Run(ctx, harness.Resume("lead-run"))
	if err != nil || res.State != harness.StateSuspended || res.Waits[0].Kind != harness.WaitUnknownOutcome {
		t.Fatalf("%v %+v", err, res)
	}
	wait := res.Waits[0]
	res, err = build(inner).Run(ctx, harness.Resume("lead-run", harness.Signal{Wait: wait.ID, Value: "pay was made"}))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	kid, err := harness.Fold(mustLoad(t, inner, wait.Run))
	if err != nil {
		t.Fatal(err)
	}
	if !kid.State.Terminal() {
		t.Fatalf("the child never finished: state %s, waits %+v", kid.State, kid.Waits)
	}
}

// failChildOnce fails one Append of one run and nothing else: a blip that a
// single-agent run would treat as a crash.
type failChildOnce struct {
	harness.Journal
	run   string
	at    int
	n     atomic.Int64
	fired atomic.Bool
}

func (f *failChildOnce) Append(ctx context.Context, id string, evs []harness.Event) error {
	if id == f.run && int(f.n.Add(1)) == f.at && !f.fired.Swap(true) {
		return errCrash
	}
	return f.Journal.Append(ctx, id, evs)
}

// A child that cannot write is a stopped worker, not a tool error. If the lead
// hears "error" its model will delegate again, under a new key, to a new child.
func TestChildJournalFailureStopsTheLead(t *testing.T) {
	ctx := context.Background()
	w := newWorld()
	inner := harness.NewMemJournal()
	childID := ""
	spy := tool.Func(tool.Spec{Name: "delegate", Effect: tool.EffectIdempotent}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		childID = tool.IdempotencyKey(ctx)
		child := &harness.Agent{Name: "child", Provider: w.provider("pay"), Tools: tool.NewRegistry(w.tool("pay", tool.EffectOnce))}
		in := harness.Prompt("sub")
		in.RunID = childID
		res, err := child.Run(ctx, in)
		if err != nil {
			return "", err
		}
		return res.Output, nil
	})
	j := &failChildOnce{Journal: inner, at: 6}
	lead := &harness.Agent{Name: "lead", Journal: j, Provider: w.provider("delegate"), Tools: tool.NewRegistry(spy)}
	in := harness.Prompt("go")
	in.RunID = "lead-run"
	j.run = "unset"
	// The key is stable, so learn it from a dry build of the same run.
	dry := harness.NewMemJournal()
	dw := newWorld()
	dryLead := &harness.Agent{Name: "lead", Journal: dry, Provider: dw.provider("delegate"), Tools: tool.NewRegistry(spy)}
	if _, err := dryLead.Run(ctx, in); err != nil {
		t.Fatal(err)
	}
	j.run = childID
	w = newWorld()
	lead.Provider = w.provider("delegate")
	res, err := lead.Run(ctx, in)
	if err == nil {
		t.Fatalf("the lead carried on after its child stopped: %+v", res)
	}
	if !errors.Is(err, harness.ErrDrained) || !errors.Is(err, errCrash) {
		t.Fatalf("want a resumable stop that keeps its cause, got %v", err)
	}
	res = driveAny(t, lead, "lead-run")
	if !res.OK() {
		t.Fatalf("%+v", res)
	}
	if n := len(w.effect["pay"]); n > 1 {
		t.Fatalf("once-only tool ran %d times", n)
	}
}

// driveAny resumes until the run ends, answering every wait as an operator would.
func driveAny(t *testing.T, a *harness.Agent, id string) *harness.Run {
	t.Helper()
	var sig []harness.Signal
	for range 10 {
		res, err := a.Run(context.Background(), harness.Resume(id, sig...))
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if res.State.Terminal() {
			return res
		}
		sig = nil
		for _, w := range res.Waits {
			sig = append(sig, harness.Signal{Wait: w.ID, Value: "verified"})
		}
	}
	t.Fatal("did not converge")
	return nil
}

// A provider cut off inside the arguments must not wedge a durable run on a
// journal that really serializes.
func TestTruncatedArgumentsAreStorable(t *testing.T) {
	ctx := context.Background()
	calls := 0
	p := model.ProviderFunc(func(_ context.Context, req model.Request) iter.Seq2[model.Event, error] {
		calls++
		if calls > 3 {
			return model.Stream(model.Response{Text: "gave up"})
		}
		return func(yield func(model.Event, error) bool) {
			if !yield(model.ToolCallDelta{Index: 0, ID: "c", Name: "x", Arguments: `{"a":`}, nil) {
				return
			}
			yield(model.Finish{Reason: model.FinishLength}, nil)
		}
	})
	x := tool.Func(tool.Spec{Name: "x"}, func(context.Context, json.RawMessage) (string, error) { return "ran", nil })
	a := &harness.Agent{Journal: newJSONJournal(), Provider: p, Tools: tool.NewRegistry(x)}
	in := harness.Prompt("go")
	in.RunID = "t"
	res, err := a.Run(ctx, in)
	if err != nil {
		t.Fatalf("worker stopped on a truncated reply: %v", err)
	}
	if !res.OK() {
		t.Fatalf("%+v", res)
	}
}

// One parallel read that ends the run must not panic on its siblings' results.
func TestHandoffInAParallelGroup(t *testing.T) {
	r1 := tool.Func(tool.Spec{Name: "r1", Effect: tool.EffectRead}, func(context.Context, json.RawMessage) (string, error) {
		return "", &tool.Handoff{Target: "other"}
	})
	release := make(chan struct{})
	r2 := tool.Func(tool.Spec{Name: "r2", Effect: tool.EffectRead}, func(context.Context, json.RawMessage) (string, error) {
		<-release
		return "late", nil
	})
	w := newWorld()
	sink := func(e harness.Event) {
		if e.Kind == harness.KindToolDone {
			close(release)
		}
	}
	a := &harness.Agent{Provider: w.provider("r1", "r2"), Tools: tool.NewRegistry(r1, r2)}
	res, err := a.Run(harness.WithEvents(context.Background(), sink), harness.Prompt("go"))
	if err != nil || res.Stop != harness.StopHandoff {
		t.Fatalf("%v %+v", err, res)
	}
}

type scrubber struct{}

const raw = "756.1234.5678.97"

func (scrubber) Name() string { return "scrubber" }
func (scrubber) Redact(_ context.Context, _ harness.Kind, s string) (string, error) {
	return strings.ReplaceAll(s, raw, "[AHV]"), nil
}

// A child inherits the journal and the sinks, so it must inherit what keeps them
// clean.
func TestChildInheritsTheLeadsRedactor(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	w := newWorld()
	leak := tool.Func(tool.Spec{Name: "leak"}, func(context.Context, json.RawMessage) (string, error) { return "customer " + raw, nil })
	child := &harness.Agent{Name: "child", Provider: w.provider("leak"), Tools: tool.NewRegistry(leak)}
	key := ""
	del := tool.Func(tool.Spec{Name: "delegate", Effect: tool.EffectIdempotent}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		key = tool.IdempotencyKey(ctx)
		in := harness.Prompt("sub")
		in.RunID = key
		res, err := child.Run(ctx, in)
		if err != nil {
			return "", err
		}
		return res.Output, nil
	})
	lead := &harness.Agent{Name: "lead", Journal: j, Provider: w.provider("delegate"), Tools: tool.NewRegistry(del), Hooks: []harness.Hook{scrubber{}}}
	in := harness.Prompt("go")
	in.RunID = "r"
	if _, err := lead.Run(ctx, in); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(mustLoad(t, j, key))
	if strings.Contains(string(b), raw) {
		t.Fatal("the shared journal holds the raw value from a child run")
	}
}

// A tool that parks twice hears the answer to its latest ask, not the old one. An old
// answer forwarded to a child would make the child refuse the whole delivery.
func TestAToolHearsOnlyTheAnswersToItsLatestPark(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	w := newWorld()
	var heard [][]string
	twice := tool.Func(tool.Spec{Name: "twice", Effect: tool.EffectIdempotent}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		var values []string
		for _, s := range harness.Signals(ctx) {
			values = append(values, s.Value)
		}
		heard = append(heard, values)
		if len(heard) < 3 {
			return "", harness.Suspend(harness.Wait{Kind: "more"})
		}
		return "done", nil
	})
	a := &harness.Agent{Journal: j, Provider: w.provider("twice"), Tools: tool.NewRegistry(twice)}
	in := harness.Prompt("x")
	in.RunID = "r"
	res, err := a.Run(ctx, in)
	for _, v := range []string{"first", "second"} {
		if err != nil {
			t.Fatal(err)
		}
		res, err = a.Run(ctx, harness.Resume("r", harness.Signal{Wait: res.Waits[0].ID, Value: v}))
	}
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if got := heard[2]; len(got) != 1 || got[0] != "second" {
		t.Fatalf("the last invocation heard %v", got)
	}
}

// A wait a parent copies from a child expires when the child's does. With a journal
// that keeps the input-only TTL, forwarding must not start the clock again.
func TestAForwardedWaitKeepsItsExpiry(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	w := newWorld()
	child := approvalAgent(w, nil, time.Hour)
	child.Journal = nil
	lead := &harness.Agent{Journal: j, Provider: w.provider("delegate"), Tools: tool.NewRegistry(subagentTool(child, "delegate"))}
	in := harness.Prompt("go")
	in.RunID = "root"
	res, err := lead.Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	kid, err := harness.Fold(mustLoad(t, j, res.Waits[0].Run))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Waits[0].Expires.Equal(kid.Waits[0].Expires) {
		t.Fatalf("root %v child %v", res.Waits[0].Expires, kid.Waits[0].Expires)
	}
}

type failLoad struct {
	harness.Journal
	root  string
	fired atomic.Bool
}

func (f *failLoad) Load(ctx context.Context, id string) ([]harness.Event, error) {
	if id != f.root && !f.fired.Swap(true) {
		return nil, errCrash
	}
	return f.Journal.Load(ctx, id)
}

func TestChildLoadFailureStopsTheLead(t *testing.T) {
	ctx := context.Background()
	w := newWorld()
	child := &harness.Agent{Name: "child", Provider: w.provider("pay"), Tools: tool.NewRegistry(w.tool("pay", tool.EffectOnce))}
	j := &failLoad{Journal: harness.NewMemJournal(), root: "root"}
	lead := &harness.Agent{Name: "lead", Journal: j, Provider: w.provider("delegate"), Tools: tool.NewRegistry(subagentTool(child, "delegate"))}
	in := harness.Prompt("go")
	in.RunID = "root"
	if _, err := lead.Run(ctx, in); !errors.Is(err, harness.ErrDrained) {
		t.Fatalf("want a resumable stop, got %v", err)
	}
	if res := driveAny(t, lead, "root"); !res.OK() {
		t.Fatalf("%+v", res)
	}
}
