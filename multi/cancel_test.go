package multi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/multi"
	"github.com/ecociel/agent-harness/tool"
)

// closings counts AfterRun, the hook a cancel must not silently skip.
type closings struct {
	mu sync.Mutex
	n  int
}

func (*closings) Name() string { return "closings" }
func (c *closings) AfterRun(context.Context, harness.Run) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}
func (c *closings) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// turns is a model that calls each tool in its own turn, then answers, so a stopped
// run has something left it would have paid for.
func (w *world) turns(agent string, tools ...string) model.Provider {
	return model.ProviderFunc(func(_ context.Context, req model.Request) iter.Seq2[model.Event, error] {
		w.mu.Lock()
		w.paid++
		w.mu.Unlock()
		done := 0
		for _, m := range req.Messages {
			if m.Role == model.RoleTool {
				done++
			}
		}
		if done == len(tools) {
			return model.Stream(model.Response{Text: agent + " done", Usage: model.Usage{Input: 10, Output: 1}})
		}
		return model.Stream(model.Response{Usage: model.Usage{Input: 10, Output: 1},
			ToolCalls: []model.ToolCall{{ID: fmt.Sprint("c", done), Name: tools[done], Arguments: json.RawMessage(`{"task":"go"}`)}}})
	})
}

// starter is a tool a consumer wrote: it starts a child under a name of its own and
// implements nothing else, so no walk can find that child.
func starter(name, id string, child harness.Runner) tool.Tool {
	return tool.Func(tool.Spec{Name: name, Effect: tool.EffectIdempotent}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		in := harness.Prompt("sub task")
		in.RunID = id
		res, err := child.Run(ctx, in)
		if err != nil {
			return "", err
		}
		return res.Output, nil
	})
}

// async runs f on its own goroutine and hands back its error.
func async(f func() error) chan error {
	done := make(chan error, 1)
	go func() { done <- f() }()
	return done
}

func cancel(t *testing.T, r harness.Runner, id string) *harness.Run {
	t.Helper()
	res, err := r.Run(context.Background(), harness.Input{RunID: id, Cancel: "stop now"})
	if err != nil || res.Stop != harness.StopCancelled {
		t.Fatalf("cancel %s: %v %+v", id, err, res)
	}
	return res
}

func cancelled(t *testing.T, j *jsonJournal, id string) *harness.Run {
	t.Helper()
	r := fold(t, j, id)
	if r.Stop != harness.StopCancelled || !r.State.Terminal() {
		t.Fatalf("%s did not end cancelled: %s %s %q", id, r.State, r.Stop, r.Detail)
	}
	return r
}

// A tool the harness knows nothing about started this child. Nobody can walk to it,
// so it reads the cancel itself, before its next call.
func TestCancelStopsAChildNoWalkCanFindBeforeItsNextModelCall(t *testing.T) {
	j := newJournal()
	w := newWorld()
	started, release := make(chan struct{}), make(chan struct{})
	child := &harness.Agent{Name: "child", Provider: w.turns("child", "pay", "next"),
		Tools: tool.NewRegistry(blocking(w, "pay", started, release), w.tool("next", tool.Spec{Effect: tool.EffectOnce}))}
	root := &harness.Agent{Name: "root", Journal: j, Provider: w.turns("root", "start"),
		Tools: tool.NewRegistry(starter("start", "a-name-of-its-own", child))}
	in := harness.Prompt("x")
	in.RunID = "root"
	worker := async(func() error { _, err := root.Run(context.Background(), in); return err })
	<-started
	cancel(t, root, "root")
	if r := fold(t, j, "a-name-of-its-own"); r.State != harness.StateTools {
		t.Fatalf("the walk found a child it should not be able to: %s", r.State)
	}
	paid := w.billed()
	close(release)
	if err := <-worker; !errors.Is(err, harness.ErrFenced) {
		t.Fatalf("got %v", err)
	}
	r := cancelled(t, j, "a-name-of-its-own")
	if r.Detail != "stop now" {
		t.Fatalf("detail %q", r.Detail)
	}
	if w.billed() != paid {
		t.Fatalf("the child paid for %d calls after the cancel", w.billed()-paid)
	}
	if w.count("next") != 0 {
		t.Fatal("the child started another tool")
	}
}

// The model call was in flight at the cancel, so its reply is recorded. The tool it
// asked for has only an intent: the check after the intent stops the effect.
func TestCancelStopsAChildNoWalkCanFindBeforeItsNextTool(t *testing.T) {
	j := newJournal()
	w := newWorld()
	started, release := make(chan struct{}), make(chan struct{})
	slow := model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
		close(started)
		<-release
		return w.provider("child", "pay").Complete(ctx, req)
	})
	child := &harness.Agent{Name: "child", Provider: slow, Tools: tool.NewRegistry(w.tool("pay", tool.Spec{Effect: tool.EffectOnce}))}
	root := &harness.Agent{Name: "root", Journal: j, Provider: w.turns("root", "start"),
		Tools: tool.NewRegistry(starter("start", "a-name-of-its-own", child))}
	in := harness.Prompt("x")
	in.RunID = "root"
	worker := async(func() error { _, err := root.Run(context.Background(), in); return err })
	<-started
	cancel(t, root, "root")
	close(release)
	if err := <-worker; !errors.Is(err, harness.ErrFenced) {
		t.Fatalf("got %v", err)
	}
	r := cancelled(t, j, "a-name-of-its-own")
	if w.count("pay") != 0 || r.ToolCalls != 0 {
		t.Fatalf("a cancelled child ran its tool: pay %d", w.count("pay"))
	}
}

// Two plain tools hide the middle run from the walk and leave it uncancelled, so only
// reading past the parent finds the root.
func TestCancelReachesPastAnAncestorThatDoesNotKnow(t *testing.T) {
	j := newJournal()
	w := newWorld()
	started, release := make(chan struct{}), make(chan struct{})
	leaf := &harness.Agent{Name: "leaf", Provider: w.turns("leaf", "pay", "next"),
		Tools: tool.NewRegistry(blocking(w, "pay", started, release), w.tool("next", tool.Spec{Effect: tool.EffectOnce}))}
	mid := &harness.Agent{Name: "mid", Provider: w.turns("mid", "start"), Tools: tool.NewRegistry(starter("start", "leaf-run", leaf))}
	root := &harness.Agent{Name: "root", Journal: j, Provider: w.turns("root", "start"), Tools: tool.NewRegistry(starter("start", "mid-run", mid))}
	in := harness.Prompt("x")
	in.RunID = "root"
	worker := async(func() error { _, err := root.Run(context.Background(), in); return err })
	<-started
	cancel(t, root, "root")
	paid := w.billed()
	close(release)
	if err := <-worker; !errors.Is(err, harness.ErrFenced) {
		t.Fatalf("got %v", err)
	}
	cancelled(t, j, "leaf-run")
	if w.billed() != paid || w.count("next") != 0 {
		t.Fatalf("paid %d more, next ran %d times", w.billed()-paid, w.count("next"))
	}
}

// The cancel went by before this step existed, so no walk could have ended it. The
// worker that is still alive starts it anyway, and it stops before paying.
func TestCancelStopsAStepThatHadNotStarted(t *testing.T) {
	j := newJournal()
	w := newWorld()
	started, release := make(chan struct{}), make(chan struct{})
	first := harness.RunnerFunc(func(_ context.Context, in harness.Input) (*harness.Run, error) {
		if in.Cancel != "" {
			return nil, harness.ErrNoSuchRun
		}
		close(started)
		<-release
		return &harness.Run{State: harness.StateDone, Stop: harness.StopCompleted, Output: "first"}, nil
	})
	build := func() *multi.Sequential {
		return &multi.Sequential{Journal: j, Steps: []harness.Runner{first, w.agent("b", "pay")}}
	}
	in := harness.Prompt("x")
	in.RunID = "root"
	worker := async(func() error { _, err := build().Run(context.Background(), in); return err })
	<-started
	cancel(t, build(), "root")
	if ids := j.ids(); len(ids) != 1 {
		t.Fatalf("the walk started something: %v", ids)
	}
	close(release)
	if err := <-worker; !errors.Is(err, harness.ErrFenced) {
		t.Fatalf("got %v", err)
	}
	cancelled(t, j, "root/1")
	if w.billed() != 0 || w.count("pay") != 0 {
		t.Fatalf("a step that started after the cancel paid %d and ran %d", w.billed(), w.count("pay"))
	}
}

func TestCancelThroughAnAgentRootStopsARunningChild(t *testing.T) {
	j := newJournal()
	w := newWorld()
	started, release := make(chan struct{}), make(chan struct{})
	child := &harness.Agent{Name: "child", Provider: w.turns("child", "pay", "next"),
		Tools: tool.NewRegistry(blocking(w, "pay", started, release), w.tool("next", tool.Spec{Effect: tool.EffectOnce}))}
	build := func() *harness.Agent { return lead(w, j, child) }
	in := harness.Prompt("x")
	in.RunID = "root"
	worker := async(func() error { _, err := build().Run(context.Background(), in); return err })
	<-started
	cancel(t, build(), "root")
	var childID string
	for _, id := range j.ids() {
		if id != "root" {
			childID = id
		}
	}
	cancelled(t, j, childID)
	paid := w.billed()
	close(release)
	if err := <-worker; !errors.Is(err, harness.ErrFenced) {
		t.Fatalf("got %v", err)
	}
	if w.billed() != paid || w.count("next") != 0 {
		t.Fatalf("paid %d more, next ran %d times", w.billed()-paid, w.count("next"))
	}
}

// The approval path, cancelled. The child is parked, so only the walk can end it, and
// the walk goes through the same tool a signal does, to every depth.
func TestCancelThroughAnAgentRootEndsAParkedTree(t *testing.T) {
	ctx := context.Background()
	j := newJournal()
	w := newWorld()
	closed := &closings{}
	build := func() *harness.Agent {
		return lead(w, j, &multi.Sequential{Name: "pipe", Steps: []harness.Runner{w.agent("child", "send", asks(), closed)}})
	}
	in := harness.Prompt("x")
	in.RunID = "root"
	res, err := build().Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	wait := res.Waits[0]
	paid := w.billed()
	cancel(t, build(), "root")
	cancelled(t, j, wait.Run)
	for _, id := range j.ids() {
		cancelled(t, j, id)
	}
	if len(j.ids()) != 3 {
		t.Fatalf("runs %v", j.ids())
	}
	if closed.count() != 1 {
		t.Fatalf("the child's AfterRun ran %d times", closed.count())
	}
	if _, err = build().Run(ctx, harness.Resume("root", harness.Signal{Wait: wait.ID, Value: "yes"})); err != nil {
		t.Fatal(err)
	}
	if w.count("send") != 0 || w.billed() != paid {
		t.Fatalf("send ran %d times, paid %d more", w.count("send"), w.billed()-paid)
	}
}

// A finished run has nothing to cancel, so a job that outlived its lead stays out of
// reach. Pinned as a limit: cancel the job by its own ID.
func TestACancelAfterTheLeadFinishedDoesNotReachItsJob(t *testing.T) {
	j := newJournal()
	w := newWorld()
	started, release := make(chan struct{}), make(chan struct{})
	child := &harness.Agent{Name: "job", Provider: w.turns("job", "pay", "next"),
		Tools: tool.NewRegistry(blocking(w, "pay", started, release), w.tool("next", tool.Spec{Effect: tool.EffectOnce}))}
	b := multi.NewBackground(multi.Subagent{Name: "worker", Agent: child})
	root := &harness.Agent{Name: "root", Journal: j, Provider: w.provider("root", "start_worker"), Tools: tool.NewRegistry(b.Tools()...)}
	in := harness.Prompt("x")
	in.RunID = "root"
	if res, err := root.Run(context.Background(), in); err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	<-started
	res, err := root.Run(context.Background(), harness.Input{RunID: "root", Cancel: "stop now"})
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	close(release)
	b.Wait()
	if w.count("next") != 1 {
		t.Fatalf("next ran %d times", w.count("next"))
	}
}

func TestCancelEndsARunningBackgroundJobOfALiveLead(t *testing.T) {
	j := newJournal()
	w := newWorld()
	started, release := make(chan struct{}), make(chan struct{})
	child := &harness.Agent{Name: "job", Provider: w.turns("job", "pay", "next"),
		Tools: tool.NewRegistry(blocking(w, "pay", started, release), w.tool("next", tool.Spec{Effect: tool.EffectOnce}))}
	b := multi.NewBackground(multi.Subagent{Name: "worker", Agent: child})
	holding, hold := make(chan struct{}), make(chan struct{})
	held := tool.Func(tool.Spec{Name: "hold", Effect: tool.EffectIdempotent}, func(context.Context, json.RawMessage) (string, error) {
		close(holding)
		<-hold
		return "held", nil
	})
	root := &harness.Agent{Name: "root", Journal: j, Provider: w.turns("root", "start_worker", "hold"),
		Tools: tool.NewRegistry(append(b.Tools(), held)...)}
	in := harness.Prompt("x")
	in.RunID = "root"
	worker := async(func() error { _, err := root.Run(context.Background(), in); return err })
	<-started
	<-holding
	cancel(t, root, "root")
	jobs := slices.DeleteFunc(j.ids(), func(id string) bool { return id == "root" })
	if len(jobs) != 1 {
		t.Fatalf("runs %v", j.ids())
	}
	cancelled(t, j, jobs[0])
	paid := w.billed()
	close(release)
	close(hold)
	if err := <-worker; !errors.Is(err, harness.ErrFenced) {
		t.Fatalf("got %v", err)
	}
	b.Wait()
	if w.billed() != paid || w.count("next") != 0 {
		t.Fatalf("paid %d more, next ran %d times", w.billed()-paid, w.count("next"))
	}
}

func TestCancelEndsAParkedBackgroundJob(t *testing.T) {
	ctx := context.Background()
	j := newJournal()
	w := newWorld()
	b := multi.NewBackground(multi.Subagent{Name: "worker", Agent: w.agent("job", "send", asks())})
	root := &harness.Agent{Name: "root", Journal: j, Provider: w.turns("root", "start_worker", "hold"),
		Tools: tool.NewRegistry(append(b.Tools(), w.tool("hold", tool.Spec{Effect: tool.EffectOnce}))...),
		Hooks: []harness.Hook{holdOn{"hold"}}}
	in := harness.Prompt("x")
	in.RunID = "root"
	res, err := root.Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	b.Wait()
	jobs := slices.DeleteFunc(j.ids(), func(id string) bool { return id == "root" })
	if len(jobs) != 1 || fold(t, j, jobs[0]).State != harness.StateSuspended {
		t.Fatalf("the job should be parked: %v", j.ids())
	}
	cancel(t, root, "root")
	cancelled(t, j, jobs[0])
}

// holdOn parks one tool's call, so a lead can stay open while its job parks.
type holdOn struct{ name string }

func (holdOn) Name() string { return "hold" }
func (h holdOn) BeforeTool(_ context.Context, _ harness.Run, c harness.Call) harness.Decision {
	if c.Name == h.name {
		return harness.Await(harness.Wait{Kind: "hold"})
	}
	return harness.Allow()
}

// Cancelling one branch stops that branch and what is below it. Its sibling is not
// part of the cancel and goes on.
func TestCancelOfOneBranchLeavesItsSiblingAlone(t *testing.T) {
	ctx := context.Background()
	j := newJournal()
	w := newWorld()
	build := func() *multi.Parallel {
		return &multi.Parallel{Journal: j, AllowPartial: true, Branches: []harness.Runner{
			w.agent("a", "one", asks()),
			&multi.Sequential{Name: "pipe", Steps: []harness.Runner{w.agent("b", "two", asks())}},
		}}
	}
	in := harness.Prompt("x")
	in.RunID = "root"
	res, err := build().Run(ctx, in)
	if err != nil || len(res.Waits) != 2 {
		t.Fatalf("%v %+v", err, res)
	}
	pipe := &multi.Sequential{Name: "pipe", Journal: j, Steps: []harness.Runner{w.agent("b", "two", asks())}}
	cancel(t, pipe, "root/1")
	cancelled(t, j, "root/1/0")
	if r := fold(t, j, "root/0"); r.State != harness.StateSuspended {
		t.Fatalf("the sibling was touched: %s", r.State)
	}
}

// A second worker is moving the child at the same instant, so the walk loses the race
// for it. The child will meet the cancel at its next step, and the walk must go on to
// the rest.
func TestAWalkThatLosesARaceGoesOn(t *testing.T) {
	ctx := context.Background()
	j := newJournal()
	w := newWorld()
	contested := harness.RunnerFunc(func(context.Context, harness.Input) (*harness.Run, error) {
		return nil, fmt.Errorf("moved on: %w", harness.ErrFenced)
	})
	free := harness.RunnerFunc(func(context.Context, harness.Input) (*harness.Run, error) {
		return &harness.Run{State: harness.StateDone, Stop: harness.StopCompleted}, nil
	})
	parked := w.agent("b", "send", asks())
	build := func(first harness.Runner) *multi.Parallel {
		return &multi.Parallel{Journal: j, Branches: []harness.Runner{first, parked}}
	}
	in := harness.Prompt("x")
	in.RunID = "root"
	if res, err := build(free).Run(ctx, in); err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	res, err := build(contested).Run(ctx, harness.Input{RunID: "root", Cancel: "stop now"})
	if err != nil || !res.State.Terminal() {
		t.Fatalf("a lost race failed the cancel: %v %+v", err, res)
	}
	cancelled(t, j, "root/1")
}

// A journal that fails is not a lost race: the walk must not claim it finished, or
// the run ends with a child left behind.
func TestAWalkThatCannotReachAChildLeavesTheRunToBeRetried(t *testing.T) {
	ctx := context.Background()
	j := newJournal()
	w := newWorld()
	build := func(jj harness.Journal) *multi.Parallel {
		return &multi.Parallel{Journal: jj, Branches: []harness.Runner{w.agent("a", "send", asks())}}
	}
	in := harness.Prompt("x")
	in.RunID = "root"
	if res, err := build(j).Run(ctx, in); err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	// Only the child's write fails, so the root could still be marked ended. A crash
	// would stop that write too and hide a walk that swallowed the error.
	flaky := &failsOnce{Journal: j, run: "root/0"}
	if _, err := build(flaky).Run(ctx, harness.Input{RunID: "root", Cancel: "stop now"}); !errors.Is(err, errDead) {
		t.Fatalf("got %v", err)
	}
	if r := fold(t, j, "root"); r.State != harness.StateClosing {
		t.Fatalf("the root must stay open for a retry: %s", r.State)
	}
	cancel(t, build(j), "root")
	cancelled(t, j, "root/0")
}

// failsOnce refuses one run's next write and takes every other.
type failsOnce struct {
	harness.Journal
	run  string
	mu   sync.Mutex
	done bool
}

func (f *failsOnce) Append(ctx context.Context, id string, evs []harness.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == f.run && !f.done {
		f.done = true
		return errDead
	}
	return f.Journal.Append(ctx, id, evs)
}

// An ancestor that cannot be read may be saying stop, so a worker must not pay on a
// guess. Nothing is lost: the run is resumable.
func TestAnUnreadableAncestorStopsTheWorkerBeforeItPays(t *testing.T) {
	ctx := context.Background()
	j := newJournal()
	w := newWorld()
	blind := &blindTo{Journal: j, run: "root"}
	p := &multi.Parallel{Journal: blind, Branches: []harness.Runner{w.agent("a", "pay")}}
	in := harness.Prompt("x")
	in.RunID = "root"
	if _, err := p.Run(ctx, in); !errors.Is(err, harness.ErrDrained) {
		t.Fatalf("got %v", err)
	}
	if w.billed() != 0 {
		t.Fatalf("paid %d calls with no idea whether the tree was cancelled", w.billed())
	}
	res := drive(t, w, func() harness.Runner {
		return &multi.Parallel{Journal: j, Branches: []harness.Runner{w.agent("a", "pay")}}
	}, "root")
	if !res.OK() {
		t.Fatalf("%+v", res)
	}
}

// blindTo fails to read one run once its first load has passed.
type blindTo struct {
	harness.Journal
	run string
	mu  sync.Mutex
	n   int
}

func (b *blindTo) Load(ctx context.Context, id string) ([]harness.Event, error) {
	if id == b.run {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.n++; b.n > 1 {
			return nil, errDead
		}
	}
	return b.Journal.Load(ctx, id)
}

// lined answers Lineage from a table of one, as a store with a projection does, and
// counts the Loads it is spared.
type lined struct {
	harness.Journal
	mu     sync.Mutex
	loads  map[string]int
	broken bool
}

func (l *lined) Load(ctx context.Context, id string) ([]harness.Event, error) {
	l.mu.Lock()
	l.loads[id]++
	l.mu.Unlock()
	return l.Journal.Load(ctx, id)
}

func (l *lined) Lineage(ctx context.Context, id string) (harness.Lineage, error) {
	if l.broken {
		return harness.Lineage{}, errDead
	}
	evs, err := l.Journal.Load(ctx, id)
	if err != nil || len(evs) == 0 {
		return harness.Lineage{}, err
	}
	out := harness.Lineage{Parent: evs[0].ParentRun}
	if i := slices.IndexFunc(evs, harness.Event.IsCancel); i >= 0 {
		out.Cancelled, out.Reason = true, evs[i].Text
	}
	return out, nil
}

func (l *lined) count(id string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.loads[id]
}

// With a Lineage the check reads no events, past a parent that does not know, and the
// cancel lands the same. The root is loaded to start it and to cancel it, the middle
// to start it, and every other read would be the check.
func TestLineageCarriesACancelWithoutLoadingAnAncestor(t *testing.T) {
	inner := newJournal()
	j := &lined{Journal: inner, loads: map[string]int{}}
	w := newWorld()
	started, release := make(chan struct{}), make(chan struct{})
	leaf := &harness.Agent{Name: "leaf", Provider: w.turns("leaf", "pay", "next"),
		Tools: tool.NewRegistry(blocking(w, "pay", started, release), w.tool("next", tool.Spec{Effect: tool.EffectOnce}))}
	mid := &harness.Agent{Name: "mid", Provider: w.turns("mid", "start"), Tools: tool.NewRegistry(starter("start", "leaf-run", leaf))}
	root := &harness.Agent{Name: "root", Journal: j, Provider: w.turns("root", "start"), Tools: tool.NewRegistry(starter("start", "mid-run", mid))}
	in := harness.Prompt("x")
	in.RunID = "root"
	worker := async(func() error { _, err := root.Run(context.Background(), in); return err })
	<-started
	cancel(t, root, "root")
	paid := w.billed()
	close(release)
	if err := <-worker; !errors.Is(err, harness.ErrFenced) {
		t.Fatalf("got %v", err)
	}
	if r := cancelled(t, inner, "leaf-run"); r.Detail != "stop now" {
		t.Fatalf("detail %q", r.Detail)
	}
	if w.billed() != paid || w.count("next") != 0 {
		t.Fatalf("paid %d more, next ran %d times", w.billed()-paid, w.count("next"))
	}
	if j.count("root") != 2 || j.count("mid-run") != 1 {
		t.Fatalf("the check loaded events: root %d, mid %d", j.count("root"), j.count("mid-run"))
	}
}

// The same rule as a failed Load: it may be saying stop.
func TestAFailedLineageStopsTheWorkerBeforeItPays(t *testing.T) {
	j := &lined{Journal: newJournal(), loads: map[string]int{}, broken: true}
	w := newWorld()
	p := &multi.Parallel{Journal: j, Branches: []harness.Runner{w.agent("a", "pay")}}
	in := harness.Prompt("x")
	in.RunID = "root"
	if _, err := p.Run(context.Background(), in); !errors.Is(err, harness.ErrDrained) {
		t.Fatalf("got %v", err)
	}
	if w.billed() != 0 {
		t.Fatalf("paid %d calls with no idea whether the tree was cancelled", w.billed())
	}
}

// Kills a cancel at every append of every run it touches, before and after the write,
// with and without a second crash on the retry. The sender retries because it cannot
// know the cancel landed. Every run must end cancelled, nothing may be paid for or
// run, and a late answer must find nothing to move.
func TestCrashSweepOfACancel(t *testing.T) {
	ctx := context.Background()
	trees := map[string]func(w *world, j harness.Journal) harness.Runner{
		"parallel": func(w *world, j harness.Journal) harness.Runner {
			return &multi.Parallel{Name: "fan", Journal: j, Branches: []harness.Runner{w.agent("a", "one", asks()), w.agent("b", "two", asks())}}
		},
		"nested": func(w *world, j harness.Journal) harness.Runner {
			return &multi.Parallel{Name: "fan", Journal: j, Branches: []harness.Runner{
				w.agent("a", "one", asks()),
				&multi.Sequential{Name: "pipe", Steps: []harness.Runner{w.agent("b", "two", asks()), w.agent("c", "three")}},
			}}
		},
		"lead over a child": func(w *world, j harness.Journal) harness.Runner {
			return lead(w, j, w.agent("child", "one", asks()))
		},
		"lead over a pattern": func(w *world, j harness.Journal) harness.Runner {
			return lead(w, j, &multi.Parallel{Name: "fan", Branches: []harness.Runner{w.agent("a", "one", asks())}})
		},
	}
	stopNow := harness.Input{RunID: "root", Cancel: "stop now"}
	for name, build := range trees {
		t.Run(name, func(t *testing.T) {
			park := func(w *world, j harness.Journal) *harness.Run {
				in := harness.Prompt("the task")
				in.RunID = "root"
				res, err := build(w, j).Run(ctx, in)
				if err != nil || res.State != harness.StateSuspended {
					t.Fatalf("park: %v %+v", err, res)
				}
				return res
			}
			w := newWorld()
			clean := newJournal()
			park(w, clean)
			counted := kill(clean, "", -1, false)
			if _, err := build(w, counted).Run(ctx, stopNow); err != nil {
				t.Fatal(err)
			}
			cases := 0
			for run, writes := range counted.n {
				for at := 1; at <= writes; at++ {
					for _, after := range []bool{false, true} {
						for _, twice := range []bool{false, true} {
							cases++
							label := fmt.Sprintf("%s#%d_after=%v_twice=%v", run, at, after, twice)
							w := newWorld()
							j := newJournal()
							parked := park(w, j)
							paid := w.billed()
							if _, err := build(w, kill(j, run, at, after)).Run(ctx, stopNow); !errors.Is(err, errDead) {
								t.Fatalf("%s: want a crash, got %v", label, err)
							}
							if twice {
								if _, err := build(w, kill(j, "", 1, after)).Run(ctx, stopNow); err != nil && !errors.Is(err, errDead) {
									t.Fatalf("%s: second crash: %v", label, err)
								}
							}
							res, err := build(w, j).Run(ctx, stopNow)
							if err != nil || res.Stop != harness.StopCancelled {
								t.Fatalf("%s: retry: %v %+v", label, err, res)
							}
							for _, id := range j.ids() {
								if r := fold(t, j, id); !r.State.Terminal() || r.Stop != harness.StopCancelled {
									t.Errorf("%s: %s left %s %s", label, id, r.State, r.Stop)
								}
							}
							if _, err := build(w, j).Run(ctx, harness.Resume("root", answer(w, parked.Waits)...)); err != nil {
								t.Fatalf("%s: late answer: %v", label, err)
							}
							for _, tn := range []string{"one", "two", "three"} {
								if n := w.count(tn); n != 0 {
									t.Errorf("%s: %s ran %d times", label, tn, n)
								}
							}
							if w.billed() != paid {
								t.Errorf("%s: paid %d more", label, w.billed()-paid)
							}
						}
					}
				}
			}
			t.Logf("%d crash cases", cases)
			if cases == 0 {
				t.Fatal("the sweep tested nothing")
			}
		})
	}
}

// A cancel is an append like any other, so it can lose a position to a worker. It
// says so and writes nothing, and sending it again lands.
func TestACancelThatLosesARaceSaysSoAndARetryLands(t *testing.T) {
	ctx := context.Background()
	j := newJournal()
	w := newWorld()
	build := func(jj harness.Journal) *harness.Agent {
		return &harness.Agent{Name: "a", Journal: jj, Provider: w.turns("a", "one", "two"), Hooks: []harness.Hook{asks()},
			Tools: tool.NewRegistry(w.tool("one", tool.Spec{Risk: tool.RiskDangerous}), w.tool("two", tool.Spec{Risk: tool.RiskDangerous}))}
	}
	in := harness.Prompt("x")
	in.RunID = "root"
	res, err := build(j).Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	paused := &pauseOnLoad{Journal: j, run: "root", reached: make(chan struct{}), resume: make(chan struct{})}
	lost := async(func() error {
		_, err := build(paused).Run(ctx, harness.Input{RunID: "root", Cancel: "stop now"})
		return err
	})
	<-paused.reached
	if res, err = build(j).Run(ctx, harness.Resume("root", harness.Signal{Wait: res.Waits[0].ID, Value: "yes"})); err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	close(paused.resume)
	if err := <-lost; !errors.Is(err, harness.ErrFenced) {
		t.Fatalf("got %v", err)
	}
	if r := fold(t, j, "root"); r.State != harness.StateSuspended {
		t.Fatalf("the losing cancel moved the run: %s", r.State)
	}
	cancel(t, build(j), "root")
}

// A call in flight may have happened. The transcript of a stopped run can start the
// next one, and a model told the call was skipped would make it again.
func TestACallInFlightAtACancelIsNotReportedAsSkipped(t *testing.T) {
	j := newJournal()
	w := newWorld()
	started, release := make(chan struct{}), make(chan struct{})
	build := func() *harness.Agent {
		return &harness.Agent{Name: "a", Journal: j, Provider: w.provider("a", "pay"), Tools: tool.NewRegistry(blocking(w, "pay", started, release))}
	}
	in := harness.Prompt("x")
	in.RunID = "root"
	worker := async(func() error { _, err := build().Run(context.Background(), in); return err })
	<-started
	res := cancel(t, build(), "root")
	close(release)
	if err := <-worker; !errors.Is(err, harness.ErrFenced) {
		t.Fatalf("got %v", err)
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Role != model.RoleTool || !strings.Contains(last.Content, `"unknown":true`) || strings.Contains(last.Content, "skipped") {
		t.Fatalf("the transcript says %q of a call that ran", last.Content)
	}
}

type stopAfterOne struct{}

func (stopAfterOne) Name() string { return "stop" }
func (stopAfterOne) BeforeModel(_ context.Context, r harness.Run, _ *model.Request) error {
	if r.ModelCalls > 0 {
		return harness.Stop(harness.StopGuardrail, "no more")
	}
	return nil
}

// Only a cancel reaches down. A lead that stops for its own reasons has not been
// cancelled, and a job it started goes on, as it does when the lead finishes.
func TestAnotherStopAboveDoesNotStopAChild(t *testing.T) {
	j := newJournal()
	w := newWorld()
	started, release := make(chan struct{}), make(chan struct{})
	child := &harness.Agent{Name: "job", Provider: w.turns("job", "pay", "next"),
		Tools: tool.NewRegistry(blocking(w, "pay", started, release), w.tool("next", tool.Spec{Effect: tool.EffectOnce}))}
	b := multi.NewBackground(multi.Subagent{Name: "worker", Agent: child})
	root := &harness.Agent{Name: "root", Journal: j, Provider: w.turns("root", "start_worker", "never"),
		Tools: tool.NewRegistry(b.Tools()...), Hooks: []harness.Hook{stopAfterOne{}}}
	in := harness.Prompt("x")
	in.RunID = "root"
	res, err := root.Run(context.Background(), in)
	if err != nil || res.Stop != harness.StopGuardrail {
		t.Fatalf("%v %+v", err, res)
	}
	<-started
	close(release)
	b.Wait()
	if w.count("next") != 1 {
		t.Fatalf("a stop that was no cancel stopped a child: next ran %d times", w.count("next"))
	}
}

// A child that failed has nothing left to end. Its error is its own, and it must not
// stop the walk reaching the child beside it.
func TestAFailedChildDoesNotStopTheWalk(t *testing.T) {
	ctx := context.Background()
	j := newJournal()
	w := newWorld()
	build := func() *multi.Parallel {
		return &multi.Parallel{Journal: j, AllowPartial: true, Branches: []harness.Runner{broken("a"), w.agent("b", "send", asks())}}
	}
	in := harness.Prompt("x")
	in.RunID = "root"
	if res, err := build().Run(ctx, in); err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	cancel(t, build(), "root")
	cancelled(t, j, "root/1")
	if r := fold(t, j, "root/0"); r.State != harness.StateFailed {
		t.Fatalf("the failed child changed: %s", r.State)
	}
}

// The walk fences a branch while its model call is in flight. The provider bills it
// and the reply cannot be recorded, so the only honest thing the journal can say is
// that the call was lost. Otherwise the tree pays more than its own bound allows.
func TestACallInFlightAtACancelCountsAgainstTheBound(t *testing.T) {
	j := newJournal()
	w := newWorld()
	started, release := make(chan struct{}), make(chan struct{})
	slow := model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
		close(started)
		<-release
		return w.provider("a", "").Complete(ctx, req)
	})
	build := func() *multi.Parallel {
		return &multi.Parallel{Journal: j, Branches: []harness.Runner{&harness.Agent{Name: "a", Provider: slow}}}
	}
	in := harness.Prompt("x")
	in.RunID = "root"
	worker := async(func() error { _, err := build().Run(context.Background(), in); return err })
	<-started
	cancel(t, build(), "root")
	close(release)
	if err := <-worker; !errors.Is(err, harness.ErrFenced) {
		t.Fatalf("got %v", err)
	}
	calls, lost, _ := spend(t, j)
	if w.billed() != 1 || calls > w.billed() || w.billed() > calls+lost {
		t.Fatalf("billed %d outside [%d, %d]", w.billed(), calls, calls+lost)
	}
}
