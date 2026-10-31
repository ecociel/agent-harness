// Package journaltest is the conformance suite for harness.Journal. Every
// guarantee of a durable run rests on the store, and a store that gets the unique
// index or atomicity wrong fails silently, so a Journal should pass this suite
// before it holds a real run.
//
//	func TestConformance(t *testing.T) {
//		journaltest.Run(t, func(t *testing.T) harness.Journal { return open(t) })
//	}
package journaltest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/multi"
	"github.com/ecociel/agent-harness/tool"
)

// Run checks j against the Journal contract. open is called once per check and may
// return the same store each time: the suite picks a fresh run ID for every case.
func Run(t *testing.T, open func(t *testing.T) harness.Journal) {
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) { c.run(t, open(t)) })
	}
}

type check struct {
	name string
	run  func(t testing.TB, j harness.Journal)
}

var checks = []check{
	{"UnknownRunIsEmpty", unknownRunIsEmpty},
	{"EveryFieldSurvives", everyFieldSurvives},
	{"AwkwardTextSurvives", awkwardTextSurvives},
	{"EventsComeBackInOrder", eventsComeBackInOrder},
	{"TakenPositionConflicts", takenPositionConflicts},
	{"AppendIsAllOrNothing", appendIsAllOrNothing},
	{"RunsAreIndependent", runsAreIndependent},
	{"ConcurrentWritersGetOneWinner", concurrentWritersGetOneWinner},
	{"TwoWorkersRepeatNothing", twoWorkersRepeatNothing},
	{"CrashSweepOneAgent", func(t testing.TB, j harness.Journal) { sweep(t, j, oneAgent) }},
	{"CrashSweepParallel", func(t testing.TB, j harness.Journal) { sweep(t, j, parallel) }},
	{"CancelStopsARunningChild", cancelStopsARunningChild},
	{"CancelEndsAParkedBranch", cancelEndsAParkedBranch},
}

func newID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "-" + hex.EncodeToString(b)
}

func event(seq int64, text string) harness.Event {
	return harness.Event{Seq: seq, Time: time.Now(), Kind: harness.KindHistoryEdited, Text: text}
}

func load(t testing.TB, j harness.Journal, id string) []harness.Event {
	t.Helper()
	evs, err := j.Load(context.Background(), id)
	if err != nil {
		t.Fatalf("load %s: %v", id, err)
	}
	return evs
}

func unknownRunIsEmpty(t testing.TB, j harness.Journal) {
	if evs := load(t, j, newID("unknown")); len(evs) != 0 {
		t.Fatalf("an unknown run has %d events", len(evs))
	}
}

func everyFieldSurvives(t testing.TB, j harness.Journal) {
	temperature := 0.25
	full := harness.Event{
		Seq: 1, Time: time.Date(2026, 10, 30, 12, 0, 0, 123456789, time.UTC), Kind: harness.KindToolDone,
		RunID: "r", RootRun: "root", ParentRun: "parent", Agent: "a", Depth: 2, Turn: 3,
		From: harness.StateTools, To: harness.StateCompose, Wake: time.Date(2026, 10, 30, 12, 0, 1, 987654321, time.UTC),
		Hook: "h", Tool: "t", Point: harness.PointBeforeTool, Outcome: "ok", Text: "text",
		Duration: 1500 * time.Millisecond, Index: 4, Attempt: 5,
		Pin:      &harness.Pin{Instructions: "be careful", Shape: "parallel: a, b", Model: "m", MaxTurns: 8, Temperature: &temperature},
		Messages: []model.Message{model.User("hello"), {Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "c", Name: "t", Arguments: json.RawMessage(`{"a":[1,2]}`)}}}},
		Edit:     &harness.Edit{From: 1, To: 2, With: []model.Message{model.Assistant("short")}, Source: "compact"},
		Offered:  []string{"t", "u"},
		Reply:    &model.Response{Text: "answer", Usage: model.Usage{Input: 10, Output: 2}},
		Below:    &harness.Spend{Usage: model.Usage{Input: 7}, Turns: 2, ModelCalls: 3, LostCalls: 1, ToolCalls: 4},
		Waits:    []harness.Wait{{ID: "w", Run: "r", Kind: "approval", Detail: "sure?", Expires: time.Date(2026, 11, 1, 0, 0, 0, 1, time.UTC)}},
		Signal:   &harness.Signal{Wait: "w", Value: "yes", By: "op"},
		Stop:     harness.StopCompleted,
	}
	id := newID("fields")
	if err := j.Append(context.Background(), id, []harness.Event{full}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got := load(t, j, id)
	if len(got) != 1 {
		t.Fatalf("got %d events", len(got))
	}
	want, _ := json.Marshal(full)
	have, _ := json.Marshal(got[0])
	if !bytes.Equal(want, have) {
		t.Fatalf("an event changed in the store\nwrote %s\n read %s", want, have)
	}
}

// A tool's output is arbitrary, so the store must not need it to be clean text.
func awkwardTextSurvives(t testing.TB, j harness.Journal) {
	texts := []string{
		"nul\x00byte", "\xff\xfe not utf-8", "emoji 🦀 and  ", `quote " back\slash`, "'; DROP TABLE journal; --",
		"", strings.Repeat("large ", 200_000),
	}
	id := newID("text")
	var evs []harness.Event
	for i, s := range texts {
		evs = append(evs, event(int64(i+1), s))
	}
	if err := j.Append(context.Background(), id, evs); err != nil {
		t.Fatalf("append: %v", err)
	}
	got := load(t, j, id)
	if len(got) != len(texts) {
		t.Fatalf("got %d of %d events", len(got), len(texts))
	}
	for i, s := range texts {
		// encoding/json replaces invalid UTF-8, so that is the most a store can keep.
		want, _ := json.Marshal(s)
		have, _ := json.Marshal(got[i].Text)
		if !bytes.Equal(want, have) {
			t.Errorf("text %d changed: %.40q became %.40q", i, s, got[i].Text)
		}
	}
}

func eventsComeBackInOrder(t testing.TB, j harness.Journal) {
	id := newID("order")
	const n = 40
	for i := int64(1); i <= n; i += 2 {
		if err := j.Append(context.Background(), id, []harness.Event{event(i, "a"), event(i+1, "b")}); err != nil {
			t.Fatalf("append at %d: %v", i, err)
		}
	}
	got := load(t, j, id)
	if len(got) != n {
		t.Fatalf("got %d events", len(got))
	}
	for i, e := range got {
		if e.Seq != int64(i+1) {
			t.Fatalf("position %d holds seq %d", i, e.Seq)
		}
	}
}

func takenPositionConflicts(t testing.TB, j harness.Journal) {
	ctx := context.Background()
	id := newID("taken")
	if err := j.Append(ctx, id, []harness.Event{event(1, "first")}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := j.Append(ctx, id, []harness.Event{event(1, "second")}); !errors.Is(err, harness.ErrConflict) {
		t.Fatalf("a taken position must return ErrConflict, got %v", err)
	}
	if got := load(t, j, id); len(got) != 1 || got[0].Text != "first" {
		t.Fatalf("the loser changed the journal: %+v", got)
	}
}

func appendIsAllOrNothing(t testing.TB, j harness.Journal) {
	ctx := context.Background()
	id := newID("atomic")
	if err := j.Append(ctx, id, []harness.Event{event(1, "a"), event(2, "b"), event(3, "c")}); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Position 3 is taken, so position 4 must not land either.
	err := j.Append(ctx, id, []harness.Event{event(3, "x"), event(4, "y")})
	if !errors.Is(err, harness.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	if got := load(t, j, id); len(got) != 3 {
		t.Fatalf("a failed append left %d events", len(got))
	}
}

func runsAreIndependent(t testing.TB, j harness.Journal) {
	ctx := context.Background()
	a, b := newID("a"), newID("b")
	for _, id := range []string{a, b} {
		if err := j.Append(ctx, id, []harness.Event{event(1, id)}); err != nil {
			t.Fatalf("the same seq in another run must not conflict: %v", err)
		}
	}
	if got := load(t, j, a); len(got) != 1 || got[0].Text != a {
		t.Fatalf("run %s holds %+v", a, got)
	}
}

// This is the fence: however many workers race, one wins each position.
func concurrentWritersGetOneWinner(t testing.TB, j harness.Journal) {
	const positions, writers = 30, 8
	ctx := context.Background()
	id := newID("race")
	for seq := int64(1); seq <= positions; seq++ {
		var won atomic.Int64
		var wg sync.WaitGroup
		errs := make(chan error, writers)
		for w := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := j.Append(ctx, id, []harness.Event{event(seq, fmt.Sprintf("writer %d", w))})
				switch {
				case err == nil:
					won.Add(1)
				case !errors.Is(err, harness.ErrConflict):
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("a losing writer must see ErrConflict: %v", err)
		}
		if won.Load() != 1 {
			t.Fatalf("position %d had %d winners", seq, won.Load())
		}
	}
	if got := load(t, j, id); len(got) != positions {
		t.Fatalf("got %d events", len(got))
	}
}

// twoWorkersRepeatNothing races two workers over one run through the real machine.
func twoWorkersRepeatNothing(t testing.TB, j harness.Journal) {
	const rounds = 10
	for range rounds {
		w := newWorld()
		id := newID("workers")
		a := w.agent("a", "pay")
		a.Journal = j
		in := harness.Prompt("the task")
		in.RunID = id
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := a.Run(context.Background(), in)
				if err != nil && !errors.Is(err, harness.ErrFenced) && !errors.Is(err, harness.ErrDrained) {
					t.Errorf("worker: %v", err)
				}
			}()
		}
		wg.Wait()
		if n := w.count("pay"); n > 1 {
			t.Fatalf("a once-only tool ran %d times with two workers: %v", n, w.ran["pay"])
		}
		finish(t, w, func() harness.Runner { return a }, id, harness.Resume(id))
		if n := w.count("pay"); n != 1 {
			t.Fatalf("a once-only tool ran %d times after recovery", n)
		}
	}
}

// parks asks for a person before any tool runs.
type parks struct{}

func (parks) Name() string { return "parks" }
func (parks) BeforeTool(context.Context, harness.Run, harness.Call) harness.Decision {
	return harness.Await(harness.Wait{Kind: "approval"})
}

// A cancel reaches a parked branch through the store, and a late answer moves nothing.
func cancelEndsAParkedBranch(t testing.TB, j harness.Journal) {
	ctx := context.Background()
	w := newWorld()
	id := newID("cancel-parked")
	build := func() harness.Runner {
		a := w.agent("a", "pay")
		a.Hooks = []harness.Hook{parks{}}
		return &multi.Parallel{Name: "both", Journal: j, Branches: []harness.Runner{a}}
	}
	in := harness.Prompt("the task")
	in.RunID = id
	res, err := build().Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("start: %v %+v", err, res)
	}
	wait := res.Waits[0]
	if res, err = build().Run(ctx, harness.Input{RunID: id, Cancel: "stop now"}); err != nil || res.Stop != harness.StopCancelled {
		t.Fatalf("cancel: %v %+v", err, res)
	}
	branch, err := harness.Fold(load(t, j, id+"/0"))
	if err != nil || branch.Stop != harness.StopCancelled || !branch.State.Terminal() {
		t.Fatalf("the parked branch did not end: %v %+v", err, branch)
	}
	if _, err := build().Run(ctx, harness.Resume(id, harness.Signal{Wait: wait.ID, Value: "yes"})); err != nil {
		t.Fatalf("late answer: %v", err)
	}
	if n := w.count("pay"); n != 0 {
		t.Fatalf("a cancelled branch ran its tool %d times", n)
	}
}

// A child under a name of its own reads the cancel from the store, past a parent that
// holds it in a tool, so it depends on Load showing another run's latest event.
func cancelStopsARunningChild(t testing.TB, j harness.Journal) {
	ctx := context.Background()
	w := newWorld()
	id := newID("cancel-running")
	childID := id + "-child"
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	first := tool.Func(tool.Spec{Name: "first", Effect: tool.EffectIdempotent}, func(context.Context, json.RawMessage) (string, error) {
		close(started)
		<-release
		return "first ok", nil
	})
	child := &harness.Agent{Name: "child", Tools: tool.NewRegistry(first), Provider: w.provider("child", "first")}
	start := tool.Func(tool.Spec{Name: "start", Effect: tool.EffectIdempotent}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		in := harness.Prompt("sub task")
		in.RunID = childID
		res, err := child.Run(ctx, in)
		if err != nil {
			return "", err
		}
		return res.Output, nil
	})
	root := &harness.Agent{Name: "root", Journal: j, Tools: tool.NewRegistry(start), Provider: w.provider("root", "start")}
	in := harness.Prompt("the task")
	in.RunID = id
	done := make(chan error, 1)
	go func() {
		_, err := root.Run(ctx, in)
		done <- err
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("the child never started: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the child never started")
	}
	if res, err := root.Run(ctx, harness.Input{RunID: id, Cancel: "stop now"}); err != nil || res.Stop != harness.StopCancelled {
		t.Fatalf("cancel: %v %+v", err, res)
	}
	paid := w.billed()
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		if !errors.Is(err, harness.ErrFenced) {
			t.Fatalf("the root's worker must be fenced, got %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the worker never stopped")
	}
	r, err := harness.Fold(load(t, j, childID))
	if err != nil || r.Stop != harness.StopCancelled || !r.State.Terminal() {
		t.Fatalf("the child did not stop: %v %+v", err, r)
	}
	if w.billed() != paid {
		t.Fatalf("the child paid for %d calls after the cancel", w.billed()-paid)
	}
}

var errDead = errors.New("process died")

// crash kills the process at one append of one run: before the write, or after it
// with the caller never hearing back. Every later write fails too.
type crash struct {
	harness.Journal
	run   string // "" counts every run
	at    int
	after bool

	mu   sync.Mutex
	n    map[string]int
	dead bool
}

func kill(j harness.Journal, run string, at int, after bool) *crash {
	return &crash{Journal: j, run: run, at: at, after: after, n: map[string]int{}}
}

func (c *crash) Append(ctx context.Context, id string, evs []harness.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead {
		return errDead
	}
	c.n[id]++
	if (c.run == "" || c.run == id) && c.n[id] == c.at {
		c.dead = true
		if c.after {
			_ = c.Journal.Append(ctx, id, evs)
		}
		return errDead
	}
	return c.Journal.Append(ctx, id, evs)
}

// recorder notes every run in a tree, because Journal has no way to list them.
type recorder struct {
	harness.Journal
	mu  sync.Mutex
	ids map[string]bool
}

func (r *recorder) Append(ctx context.Context, id string, evs []harness.Event) error {
	r.mu.Lock()
	r.ids[id] = true
	r.mu.Unlock()
	return r.Journal.Append(ctx, id, evs)
}

func (r *recorder) runs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.ids))
	for id := range r.ids {
		out = append(out, id)
	}
	return out
}

// world is the outside: what tools really did and what providers really billed.
type world struct {
	mu   sync.Mutex
	paid int
	ran  map[string][]string
}

func newWorld() *world { return &world{ran: map[string][]string{}} }

func (w *world) count(name string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.ran[name])
}

func (w *world) billed() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.paid
}

// operator is the person answering an unknown outcome. They look at the outside
// world, and finish the job by hand if it never happened.
func (w *world) operator(name string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.ran[name]) == 0 {
		w.ran[name] = []string{"by hand"}
	}
	return name + " ok"
}

func (w *world) agent(name, toolName string) *harness.Agent {
	pay := tool.Func(tool.Spec{Name: toolName, Effect: tool.EffectOnce}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.ran[toolName] = append(w.ran[toolName], tool.IdempotencyKey(ctx))
		return toolName + " ok", nil
	})
	return &harness.Agent{Name: name, Provider: w.provider(name, toolName), Tools: tool.NewRegistry(pay)}
}

// provider decides from the request alone, like a model. Its answer quotes the
// prompt and the tool's result, so a lost input or result fails the comparison.
func (w *world) provider(agent, toolName string) model.Provider {
	return model.ProviderFunc(func(_ context.Context, req model.Request) iter.Seq2[model.Event, error] {
		w.mu.Lock()
		w.paid++
		w.mu.Unlock()
		for _, m := range req.Messages {
			if m.Role == model.RoleTool {
				return model.Stream(model.Response{Usage: model.Usage{Input: 10, Output: 1},
					Text: fmt.Sprintf("%s heard %q and got %q", agent, model.LastUser(req.Messages), m.Content)})
			}
		}
		return model.Stream(model.Response{Usage: model.Usage{Input: 10, Output: 1},
			ToolCalls: []model.ToolCall{{ID: "c1", Name: toolName, Arguments: json.RawMessage(`{}`)}}})
	})
}

func answer(w *world, waits []harness.Wait) []harness.Signal {
	var out []harness.Signal
	for _, wt := range waits {
		name := strings.TrimSuffix(strings.TrimPrefix(wt.Detail, "the outcome of "), " is unknown")
		out = append(out, harness.Signal{Wait: wt.ID, Value: w.operator(name), By: "op"})
	}
	return out
}

// finish runs a run to its end, answering every wait, with a fresh process for each
// call.
func finish(t testing.TB, w *world, build func() harness.Runner, id string, in harness.Input) *harness.Run {
	t.Helper()
	for range 12 {
		res, err := build().Run(context.Background(), in)
		if err != nil {
			t.Fatalf("drive %s: %v", id, err)
		}
		if res.State.Terminal() {
			return res
		}
		in = harness.Resume(id, answer(w, res.Waits)...)
	}
	t.Fatalf("%s did not converge", id)
	return nil
}

// scenario is a tree of runs a sweep can kill anywhere.
type scenario struct {
	tools []string
	build func(w *world, j harness.Journal) harness.Runner
}

var oneAgent = scenario{tools: []string{"pay"}, build: func(w *world, j harness.Journal) harness.Runner {
	a := w.agent("solo", "pay")
	a.Journal = j
	return a
}}

var parallel = scenario{tools: []string{"pay", "ship"}, build: func(w *world, j harness.Journal) harness.Runner {
	return &multi.Parallel{Name: "both", Journal: j, Branches: []harness.Runner{w.agent("left", "pay"), w.agent("right", "ship")}}
}}

// advance runs until the run ends, with a fresh process for each call. sent holds
// the last signals delivered, because a sender that crashed cannot know if they
// landed.
func (s scenario) advance(w *world, j harness.Journal, id string, in harness.Input, sent *[]harness.Signal) (*harness.Run, error) {
	var res *harness.Run
	var err error
	for range 12 {
		*sent = in.Signals
		if res, err = s.build(w, j).Run(context.Background(), in); res == nil || err != nil {
			return res, err
		}
		if res.State.Terminal() {
			return res, nil
		}
		in = harness.Resume(id, answer(w, res.Waits)...)
	}
	return res, errors.New("did not converge")
}

// sweep kills the scenario before and after every append of every run in the tree,
// restarts by run ID alone, and holds the result to the uncrashed run.
func sweep(t testing.TB, j harness.Journal, s scenario) {
	root := newID("clean")
	in := harness.Prompt("the task")
	in.RunID = root
	counted := kill(j, "", -1, false)
	clean, err := s.advance(newWorld(), counted, root, in, new([]harness.Signal))
	if err != nil || !clean.OK() {
		t.Fatalf("uncrashed run: %v %+v", err, clean)
	}
	unknown := 0
	for run, writes := range counted.n {
		for at := 1; at <= writes; at++ {
			for _, after := range []bool{false, true} {
				unknown += s.check(t, j, clean.Output, strings.TrimPrefix(run, root), at, after)
			}
		}
	}
	if unknown == 0 {
		t.Error("no case reached an unknown outcome, so the sweep did not test one")
	}
}

func (s scenario) check(t testing.TB, base harness.Journal, want, suffix string, at int, after bool) (unknown int) {
	t.Helper()
	id := newID("crash")
	name := fmt.Sprintf("%s%s#%d after=%v", id, suffix, at, after)
	w := newWorld()
	j := &recorder{Journal: base, ids: map[string]bool{}}
	in := harness.Prompt("the task")
	in.RunID = id
	var sent []harness.Signal
	if _, err := s.advance(w, kill(j, id+suffix, at, after), id, in, &sent); !errors.Is(err, harness.ErrDrained) {
		t.Fatalf("%s: a crash must read as a resumable stop, got %v", name, err)
	}
	for _, tn := range s.tools {
		if n := w.count(tn); n > 1 {
			t.Fatalf("%s: %s ran %d times before recovery", name, tn, n)
		}
	}
	next := harness.Resume(id, sent...) // the sender retries what it cannot know landed
	if len(load(t, j, id)) == 0 {
		next = in // nothing was written: the caller repeats its request
	}
	res, err := s.build(w, j).Run(context.Background(), next)
	if err != nil {
		t.Fatalf("%s: restart: %v", name, err)
	}
	for step := 0; !res.State.Terminal(); step++ {
		if step == 12 {
			t.Fatalf("%s: did not converge: %s %+v", name, res.State, res.Waits)
		}
		if slices.ContainsFunc(res.Waits, func(wt harness.Wait) bool { return wt.Kind == harness.WaitUnknownOutcome }) {
			unknown = 1
		}
		if res, err = s.build(w, j).Run(context.Background(), harness.Resume(id, answer(w, res.Waits)...)); err != nil {
			t.Fatalf("%s: drive: %v", name, err)
		}
	}
	for _, tn := range s.tools {
		if n := w.count(tn); n != 1 {
			t.Errorf("%s: once-only tool %s ran %d times: %v", name, tn, n, w.ran[tn])
		}
	}
	if res.Stop != harness.StopCompleted || res.Output != want {
		t.Errorf("%s: %s %q\nwant %q", name, res.Stop, res.Output, want)
	}
	var calls, lost int
	for _, run := range j.runs() {
		r, err := harness.Fold(load(t, j, run))
		if err != nil {
			t.Fatalf("%s: fold %s: %v", name, run, err)
		}
		if !r.State.Terminal() {
			t.Errorf("%s: run %s left in %s", name, run, r.State)
		}
		calls += r.ModelCalls
		lost += r.LostCalls
	}
	if calls > w.billed() || w.billed() > calls+lost {
		t.Errorf("%s: billed %d outside [%d, %d]", name, w.billed(), calls, calls+lost)
	}
	paid := w.billed()
	again, err := s.build(w, j).Run(context.Background(), harness.Resume(id))
	if err != nil || again.Output != res.Output || w.billed() != paid {
		t.Errorf("%s: a finished run must answer from its journal: %v %+v", name, err, again)
	}
	return unknown
}
