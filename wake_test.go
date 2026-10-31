package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/tool"
)

// sweepJournal is a consumer's store. Append projects one row per run in the same
// critical section as the write, as a transaction would, and due is the one query
// from the Journal documentation.
type sweepJournal struct {
	*jsonJournal
	mu   sync.Mutex
	rows map[string]row
}

type row struct {
	root, parent string
	state        harness.State
	wake         time.Time
}

func newSweepJournal() *sweepJournal {
	return &sweepJournal{jsonJournal: newJSONJournal(), rows: map[string]row{}}
}

func (j *sweepJournal) Append(ctx context.Context, id string, evs []harness.Event) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.jsonJournal.Append(ctx, id, evs); err != nil {
		return err
	}
	last := evs[len(evs)-1]
	j.rows[id] = row{root: last.RootRun, parent: last.ParentRun, state: last.To, wake: last.Wake}
	return nil
}

func (j *sweepJournal) due(now time.Time) []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []string
	for id, r := range j.rows {
		if r.parent != "" || r.wake.IsZero() || r.wake.After(now) {
			continue
		}
		alive := false
		for _, c := range j.rows {
			alive = alive || (c.root == id && c.wake.After(now) && c.state != harness.StateSuspended)
		}
		if !alive {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

var soon = time.Now().Add(harness.DefaultLease + time.Minute)

func TestSweepFindsACrashedWorker(t *testing.T) {
	j := newSweepJournal()
	w := newWorld()
	a := &harness.Agent{Name: "a", Journal: j, Provider: w.provider("pay"), Tools: tool.NewRegistry(w.tool("pay", tool.EffectIdempotent))}
	dying := *a
	dying.Journal = &crashJournal{Journal: j, at: 4}
	in := harness.Prompt("x")
	in.RunID = "r"
	if _, err := dying.Run(context.Background(), in); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	if due := j.due(time.Now()); len(due) != 0 {
		t.Fatalf("a worker inside its lease was swept: %v", due)
	}
	due := j.due(soon)
	if !slices.Equal(due, []string{"r"}) {
		t.Fatalf("due %v", due)
	}
	res, err := a.Run(context.Background(), harness.Resume(due[0]))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if due := j.due(soon); len(due) != 0 {
		t.Fatalf("a finished run stays due: %v", due)
	}
}

func TestSweepFindsADrainedRun(t *testing.T) {
	j := newSweepJournal()
	w := newWorld()
	ctx, drain := harness.WithDrain(context.Background())
	drainer := tool.Func(tool.Spec{Name: "pay", Effect: tool.EffectIdempotent}, func(context.Context, json.RawMessage) (string, error) {
		drain()
		return "paid", nil
	})
	a := &harness.Agent{Name: "a", Journal: j, Provider: w.provider("pay"), Tools: tool.NewRegistry(drainer)}
	in := harness.Prompt("x")
	in.RunID = "r"
	if _, err := a.Run(ctx, in); !errors.Is(err, harness.ErrDrained) {
		t.Fatalf("want drained, got %v", err)
	}
	if due := j.due(soon); !slices.Equal(due, []string{"r"}) {
		t.Fatalf("due %v", due)
	}
}

func TestSweepFindsADueTimerAndNotBefore(t *testing.T) {
	j := newSweepJournal()
	attempts := 0
	p := model.ProviderFunc(func(context.Context, model.Request) iter.Seq2[model.Event, error] {
		attempts++
		if attempts == 1 {
			return model.Fail(&model.Error{Class: model.ClassRateLimit, RetryAfter: time.Hour, Err: errors.New("429")})
		}
		return model.Stream(model.Response{Text: "ok"})
	})
	a := &harness.Agent{Name: "a", Journal: j, Provider: p, Hooks: []harness.Hook{retryAlways{}}}
	in := harness.Prompt("x")
	in.RunID = "r"
	res, err := a.Run(context.Background(), in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	if due := j.due(soon); len(due) != 0 {
		t.Fatalf("a timer an hour away was swept: %v", due)
	}
	if due := j.due(res.Waits[0].Expires.Add(time.Second)); !slices.Equal(due, []string{"r"}) {
		t.Fatalf("due %v", due)
	}
}

func TestSweepIgnoresARunOnlyASignalCanMove(t *testing.T) {
	j := newSweepJournal()
	w := newWorld()
	a := approvalAgent(w, j, 0)
	in := harness.Prompt("x")
	in.RunID = "r"
	res, err := a.Run(context.Background(), in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	if due := j.due(soon.Add(1000 * time.Hour)); len(due) != 0 {
		t.Fatalf("due %v", due)
	}
}

func TestSweepFindsAnApprovalPastItsTTL(t *testing.T) {
	j := newSweepJournal()
	w := newWorld()
	a := approvalAgent(w, j, time.Hour)
	in := harness.Prompt("x")
	in.RunID = "r"
	res, err := a.Run(context.Background(), in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	if due := j.due(soon); len(due) != 0 {
		t.Fatalf("due %v", due)
	}
	if due := j.due(res.Waits[0].Expires); !slices.Equal(due, []string{"r"}) {
		t.Fatalf("due %v", due)
	}
}

type askByName map[string]time.Duration

func (askByName) Name() string { return "ask" }
func (a askByName) BeforeTool(_ context.Context, _ harness.Run, c harness.Call) harness.Decision {
	return harness.Await(harness.Wait{Kind: "approval", TTL: a[c.Name]})
}

func TestSweepUsesTheEarliestExpiry(t *testing.T) {
	j := newSweepJournal()
	w := newWorld()
	a := &harness.Agent{Name: "a", Journal: j, Provider: w.provider("slow", "fast"),
		Tools: tool.NewRegistry(w.tool("slow", tool.EffectOnce), w.tool("fast", tool.EffectOnce)),
		Hooks: []harness.Hook{askByName{"slow": 9 * time.Hour, "fast": time.Hour}}}
	in := harness.Prompt("x")
	in.RunID = "r"
	res, err := a.Run(context.Background(), in)
	if err != nil || len(res.Waits) != 2 {
		t.Fatalf("%v %+v", err, res)
	}
	if due := j.due(time.Now().Add(2 * time.Hour)); !slices.Equal(due, []string{"r"}) {
		t.Fatalf("due %v", due)
	}
}

// A root goes quiet the moment it delegates. Its own Wake passes while the child is
// working, and a sweep must not fence a healthy tree.
func TestSweepLeavesAHealthyTreeAlone(t *testing.T) {
	j := newSweepJournal()
	w := newWorld()
	started, release := make(chan struct{}), make(chan struct{})
	slow := tool.Func(tool.Spec{Name: "slow", Effect: tool.EffectIdempotent}, func(context.Context, json.RawMessage) (string, error) {
		close(started)
		<-release
		return "ok", nil
	})
	child := &harness.Agent{Name: "child", Provider: w.provider("slow"), Tools: tool.NewRegistry(slow), Lease: 2 * time.Hour}
	lead := &harness.Agent{Name: "lead", Journal: j, Provider: w.provider("delegate"), Tools: tool.NewRegistry(subagentTool(child, "delegate"))}
	in := harness.Prompt("x")
	in.RunID = "r"
	done := make(chan error, 1)
	go func() {
		_, err := lead.Run(context.Background(), in)
		done <- err
	}()
	<-started
	if due := j.due(soon); len(due) != 0 {
		t.Errorf("a healthy tree was swept: %v", due)
	}
	if due := j.due(soon.Add(2 * time.Hour)); !slices.Equal(due, []string{"r"}) {
		t.Errorf("a tree whose lease ran out was not found: %v", due)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if due := j.due(soon.Add(2 * time.Hour)); len(due) != 0 {
		t.Fatalf("a finished tree stays due: %v", due)
	}
}

func TestEveryAppendOfACompositeEndsWithAWakeOrAReasonForNone(t *testing.T) {
	j := newJSONJournal()
	asks := 0
	c := &harness.Composite{Name: "c", Shape: "test", Journal: j, Lease: time.Minute,
		Body: func(context.Context, harness.Input) (string, harness.Spend, error) {
			if asks++; asks == 1 {
				return "", harness.Spend{}, harness.Suspend(harness.Wait{ID: "w", Run: "child", Kind: "approval"})
			}
			return "done", harness.Spend{}, nil
		}}
	in := harness.Prompt("x")
	in.RunID = "r"
	res, err := c.Run(context.Background(), in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	if res, err = c.Run(context.Background(), harness.Resume("r", harness.Signal{Wait: "w", Value: "go"})); err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	limit := time.Now().Add(time.Minute + time.Second)
	for _, e := range mustLoad(t, j, "r") {
		switch {
		case e.To == harness.StateSuspended || e.Kind == harness.KindRunEnded:
			if !e.Wake.IsZero() {
				t.Errorf("%s to %s should not wake: %v", e.Kind, e.To, e.Wake)
			}
		case e.Wake.IsZero() && e.Kind != harness.KindRunClaimed:
			t.Errorf("%s has no wake", e.Kind)
		case e.Wake.After(limit):
			t.Errorf("%s wakes past the Lease: %v", e.Kind, e.Wake)
		}
	}
}

func TestEveryAppendEndsWithAWakeOrAReasonForNone(t *testing.T) {
	j := newJSONJournal()
	w := newWorld()
	a := &harness.Agent{Name: "a", Journal: j, Provider: w.provider("pay"), Tools: tool.NewRegistry(w.tool("pay", tool.EffectIdempotent))}
	in := harness.Prompt("x")
	in.RunID = "r"
	if _, err := a.Run(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	evs := mustLoad(t, j, "r")
	for _, e := range evs[:len(evs)-1] {
		if e.Wake.IsZero() {
			t.Errorf("%s has no wake", e.Kind)
		}
	}
	if last := evs[len(evs)-1]; last.Kind != harness.KindRunEnded || !last.Wake.IsZero() {
		t.Errorf("a finished run still wakes: %+v", last)
	}
	again, err := harness.Fold(evs)
	if err != nil || !again.OK() {
		t.Fatalf("%v %+v", err, again)
	}
}
