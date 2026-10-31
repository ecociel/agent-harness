package multi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/approval"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/multi"
	"github.com/ecociel/agent-harness/tool"
)

// jsonJournal round-trips events through JSON, as any real store would.
type jsonJournal struct {
	mu   sync.Mutex
	runs map[string][][]byte
}

func newJournal() *jsonJournal { return &jsonJournal{runs: map[string][][]byte{}} }

func (j *jsonJournal) Append(_ context.Context, id string, evs []harness.Event) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if evs[0].Seq != int64(len(j.runs[id]))+1 {
		return harness.ErrConflict
	}
	for _, e := range evs {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		j.runs[id] = append(j.runs[id], b)
	}
	return nil
}

func (j *jsonJournal) Load(_ context.Context, id string) ([]harness.Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]harness.Event, len(j.runs[id]))
	for i, b := range j.runs[id] {
		if err := json.Unmarshal(b, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (j *jsonJournal) ids() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []string
	for id := range j.runs {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func (j *jsonJournal) dump() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	var b strings.Builder
	for _, evs := range j.runs {
		for _, e := range evs {
			b.Write(e)
		}
	}
	return b.String()
}

func fold(t *testing.T, j *jsonJournal, id string) *harness.Run {
	t.Helper()
	evs, err := j.Load(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	r, err := harness.Fold(evs)
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	return r
}

var errDead = errors.New("process died")

// crash kills the process at one append of one run: before the write, or after it
// with the caller never hearing back. Every later write, in any run, fails too.
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

// world is the outside: what tools really did and what providers really billed.
type world struct {
	mu   sync.Mutex
	paid int
	ran  map[string][]string
}

func newWorld() *world { return &world{ran: map[string][]string{}} }

func (w *world) effect(name, key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ran[name] = append(w.ran[name], key)
}

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

func (w *world) tool(name string, s tool.Spec) tool.Tool {
	s.Name = name
	return tool.Func(s, func(ctx context.Context, _ json.RawMessage) (string, error) {
		w.effect(name, tool.IdempotencyKey(ctx))
		return name + " ok", nil
	})
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

// provider decides from the request alone, like a model. Its answer quotes the
// prompt and the tool's result, so a changed input or a lost result changes the
// output and fails the comparison with the uncrashed run.
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
		if toolName == "" {
			return model.Stream(model.Response{Text: agent + " done", Usage: model.Usage{Input: 10, Output: 1}})
		}
		return model.Stream(model.Response{Usage: model.Usage{Input: 10, Output: 1},
			ToolCalls: []model.ToolCall{{ID: "c1", Name: toolName, Arguments: json.RawMessage(`{"task":"go"}`)}}})
	})
}

func (w *world) agent(name, toolName string, hooks ...harness.Hook) *harness.Agent {
	return &harness.Agent{Name: name, Provider: w.provider(name, toolName), Hooks: hooks,
		Tools: tool.NewRegistry(w.tool(toolName, tool.Spec{Effect: tool.EffectOnce, Risk: tool.RiskDangerous}))}
}

// spend is the projection a consumer would keep: a plain sum over every run in the
// journal.
func spend(t *testing.T, j *jsonJournal) (calls, lost, input int) {
	t.Helper()
	for _, id := range j.ids() {
		r := fold(t, j, id)
		calls += r.ModelCalls
		lost += r.LostCalls
		input += r.Usage.Input
	}
	return calls, lost, input
}

// drive resumes a run by ID alone until it ends, answering every wait.
func drive(t *testing.T, w *world, build func() harness.Runner, id string) *harness.Run {
	t.Helper()
	in := harness.Resume(id)
	for range 12 {
		res, err := build().Run(context.Background(), in)
		if err != nil {
			t.Fatalf("drive: %v", err)
		}
		if res.State.Terminal() {
			return res
		}
		in = harness.Resume(id, answer(w, res.Waits)...)
	}
	t.Fatal("did not converge")
	return nil
}

// scenario is a tree of runs a sweep can kill anywhere.
type scenario struct {
	id          string
	tools       []string
	reportsTree bool   // the root is a pattern, so its Run sums its children
	settle      func() // waits for work a dead process would have taken with it
	build       func(w *world, j harness.Journal) harness.Runner
}

func (s scenario) input() harness.Input {
	in := harness.Prompt("the task")
	in.RunID = s.id
	return in
}

// answer is whoever is on call: yes to an approval, and the outside world's answer
// to an unknown outcome.
func answer(w *world, waits []harness.Wait) []harness.Signal {
	var out []harness.Signal
	for _, wt := range waits {
		value := approval.Approved
		if wt.Kind == harness.WaitUnknownOutcome {
			value = w.operator(strings.TrimSuffix(strings.TrimPrefix(wt.Detail, "the outcome of "), " is unknown"))
		}
		out = append(out, harness.Signal{Wait: wt.ID, Value: value, By: "op"})
	}
	return out
}

// advance runs r until it ends, answering every wait, with a fresh process for each
// call. It returns how far it got and the error that stopped it. sent holds the last
// signals delivered, because a sender that crashed cannot know if they landed.
func (s scenario) advance(ctx context.Context, w *world, j harness.Journal, in harness.Input, sent *[]harness.Signal) (*harness.Run, error) {
	var res *harness.Run
	var err error
	for range 12 {
		*sent = in.Signals
		res, err = s.build(w, j).Run(ctx, in)
		if s.settle != nil {
			s.settle()
		}
		if res == nil || err != nil {
			return res, err
		}
		if res.State.Terminal() {
			return res, nil
		}
		in = harness.Resume(s.id, answer(w, res.Waits)...)
	}
	return res, errors.New("did not converge")
}

// sweep kills the scenario before and after every append of every run in the tree,
// restarts by RunID alone, and holds the result to the uncrashed run.
func sweep(t *testing.T, s scenario) {
	ctx := context.Background()
	counted := kill(newJournal(), "", -1, false)
	cleanWorld := newWorld()
	clean, err := s.advance(ctx, cleanWorld, counted, s.input(), new([]harness.Signal))
	if err != nil || !clean.OK() {
		t.Fatalf("uncrashed run: %v %+v", err, clean)
	}
	cases, unknown := 0, 0
	for run, writes := range counted.n {
		for at := 1; at <= writes; at++ {
			for _, after := range []bool{false, true} {
				for _, twice := range []bool{false, true} {
					cases++
					name := fmt.Sprintf("%s#%d_after=%v_twice=%v", run, at, after, twice)
					unknown += s.check(t, name, clean, run, at, after, twice)
				}
			}
		}
	}
	t.Logf("%d crash cases over %d runs, %d reported an unknown outcome", cases, len(counted.n), unknown)
	if unknown == 0 {
		t.Error("no case reached an unknown outcome, so the sweep did not test one")
	}
}

func (s scenario) check(t *testing.T, name string, clean *harness.Run, run string, at int, after, twice bool) (unknown int) {
	t.Helper()
	ctx := context.Background()
	w := newWorld()
	inner := newJournal()
	var sent []harness.Signal
	_, err := s.advance(ctx, w, kill(inner, run, at, after), s.input(), &sent)
	if !errors.Is(err, errDead) {
		t.Fatalf("%s: want a crash, got %v", name, err)
	}
	if !errors.Is(err, harness.ErrDrained) {
		t.Fatalf("%s: a crash must read as a resumable stop: %v", name, err)
	}
	in := harness.Resume(s.id, sent...) // the sender retries what it cannot know landed
	if len(inner.ids()) == 0 || len(fold(t, inner, s.id).Events) == 0 {
		in = s.input() // nothing was written: the caller repeats its request
	}
	if twice {
		var again []harness.Signal
		if _, err := s.advance(ctx, w, kill(inner, "", 1, after), in, &again); err != nil && !errors.Is(err, errDead) {
			t.Fatalf("%s: second restart: %v", name, err)
		}
		if len(inner.ids()) > 0 {
			in = harness.Resume(s.id, again...)
		}
	}
	for _, name := range s.tools {
		if n := w.count(name); n > 1 {
			t.Fatalf("%s: %s ran %d times before recovery", name, name, n)
		}
	}
	res, err := s.build(w, inner).Run(ctx, in)
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
		if res, err = s.build(w, inner).Run(ctx, harness.Resume(s.id, answer(w, res.Waits)...)); err != nil {
			t.Fatalf("%s: drive: %v", name, err)
		}
	}
	if s.settle != nil {
		s.settle()
	}
	for _, tn := range s.tools {
		if n := w.count(tn); n != 1 {
			t.Errorf("%s: once-only tool %s ran %d times: %v", name, tn, n, w.ran[tn])
		}
	}
	if res.Stop != harness.StopCompleted || res.Output != clean.Output {
		t.Errorf("%s: %s %q\nwant %q", name, res.Stop, res.Output, clean.Output)
	}
	// A pattern root reports its tree. An agent root reports itself, so the bound
	// that always holds is the one a consumer can project: the whole tree's journal.
	calls, lost, input := spend(t, inner)
	if calls > w.billed() || w.billed() > calls+lost {
		t.Errorf("%s: billed %d outside [%d, %d]", name, w.billed(), calls, calls+lost)
	}
	if input != 10*calls {
		t.Errorf("%s: usage %d for %d calls", name, input, calls)
	}
	if s.reportsTree {
		if got := res.Spent(); got.ModelCalls != calls || got.LostCalls != lost || got.Usage.Input != input {
			t.Errorf("%s: the pattern reports %d/%d/%d, its tree holds %d/%d/%d", name, got.ModelCalls, got.LostCalls, got.Usage.Input, calls, lost, input)
		}
	}
	for _, id := range inner.ids() {
		if r := fold(t, inner, id); !r.State.Terminal() {
			t.Errorf("%s: run %s left in %s with %+v", name, id, r.State, r.Waits)
		}
	}
	paid := w.billed()
	again, err := s.build(w, inner).Run(ctx, harness.Resume(s.id))
	if err != nil || again.Output != res.Output || w.billed() != paid {
		t.Errorf("%s: a finished run must answer from its journal: %v %+v", name, err, again)
	}
	return unknown
}

func TestCrashSweepThroughParallel(t *testing.T) {
	sweep(t, scenario{id: "root", tools: []string{"pay_a", "pay_b"}, reportsTree: true,
		build: func(w *world, j harness.Journal) harness.Runner {
			return &multi.Parallel{Name: "fan", Journal: j,
				Branches: []harness.Runner{w.agent("a", "pay_a"), w.agent("b", "pay_b")}}
		}})
}

func TestCrashSweepThroughSequential(t *testing.T) {
	sweep(t, scenario{id: "root", tools: []string{"pay_a", "pay_b"}, reportsTree: true,
		build: func(w *world, j harness.Journal) harness.Runner {
			return &multi.Sequential{Name: "pipe", Journal: j,
				Steps: []harness.Runner{w.agent("a", "pay_a"), w.agent("b", "pay_b")}}
		}})
}

func TestCrashSweepThroughParallelWithAnAggregator(t *testing.T) {
	sweep(t, scenario{id: "root", tools: []string{"pay_a", "pay_b", "pay_c"}, reportsTree: true,
		build: func(w *world, j harness.Journal) harness.Runner {
			return &multi.Parallel{Name: "fan", Journal: j, Aggregator: w.agent("c", "pay_c"),
				Branches: []harness.Runner{w.agent("a", "pay_a"), w.agent("b", "pay_b")}}
		}})
}

func asks() harness.Hook { return &approval.Policy{Threshold: tool.RiskWrite, Ask: true} }

// lead builds a lead whose one tool is a sub-agent.
func lead(w *world, j harness.Journal, child harness.Runner) *harness.Agent {
	a := &harness.Agent{Name: "lead", Journal: j, Provider: w.provider("lead", "delegate")}
	withSub, err := multi.WithSubagents(a, multi.Subagent{Name: "delegate", Agent: child})
	if err != nil {
		panic(err)
	}
	return withSub
}

func TestCrashSweepOnAnApprovalInsideASubagent(t *testing.T) {
	sweep(t, scenario{id: "root", tools: []string{"send"},
		build: func(w *world, j harness.Journal) harness.Runner {
			return lead(w, j, w.agent("child", "send", asks()))
		}})
}

func TestCrashSweepThroughEveryLayerAtOnce(t *testing.T) {
	sweep(t, scenario{id: "root", tools: []string{"pay_a", "send", "pay_c"},
		build: func(w *world, j harness.Journal) harness.Runner {
			fan := &multi.Parallel{Name: "fan", Branches: []harness.Runner{
				w.agent("a", "pay_a"),
				&multi.Sequential{Name: "pipe", Steps: []harness.Runner{w.agent("b", "send", asks()), w.agent("c", "pay_c")}},
			}}
			return lead(w, j, fan)
		}})
}

// The approval path, step by step, across process boundaries.
func TestApprovalInsideASubagentReachesTheRootAndResumesFromAnotherProcess(t *testing.T) {
	ctx := context.Background()
	j := newJournal()
	w := newWorld()
	build := func() *harness.Agent { return lead(w, j, w.agent("child", "send", asks())) }
	in := harness.Prompt("the task")
	in.RunID = "root"
	res, err := build().Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended || len(res.Waits) != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	wait := res.Waits[0]
	if wait.Kind != approval.WaitKind || wait.Run == "root" {
		t.Fatalf("the root should see the child's wait: %+v", wait)
	}
	if w.count("send") != 0 {
		t.Fatal("ran before approval")
	}
	if r := fold(t, j, wait.Run); r.ParentRun != "root" || r.RootRun != "root" || r.State != harness.StateSuspended {
		t.Fatalf("child: %+v", r)
	}
	res, err = build().Run(ctx, harness.Resume("root", harness.Signal{Wait: wait.ID, Value: approval.Approved, By: "alice"}))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if w.count("send") != 1 {
		t.Fatalf("send ran %d times", w.count("send"))
	}
	if !strings.Contains(res.Output, "send ok") {
		t.Fatalf("the lead did not get the child's answer: %q", res.Output)
	}
	if r := fold(t, j, wait.Run); !r.OK() {
		t.Fatalf("child: %+v", r)
	}
	if _, err := build().Run(ctx, harness.Resume("root", harness.Signal{Wait: wait.ID, Value: "no"})); err != nil {
		t.Fatalf("a repeated delivery must be ignored: %v", err)
	}
}

// What happens with an approval in one branch: the other branch runs to its end and
// records it. The pattern suspends only when no branch can move. The answer from
// another process resumes the waiting branch and pays for nothing that finished.
func TestApprovalInOneBranchDoesNotHoldUpTheOther(t *testing.T) {
	ctx := context.Background()
	j := newJournal()
	w := newWorld()
	build := func() *multi.Parallel {
		return &multi.Parallel{Journal: j, Branches: []harness.Runner{w.agent("a", "send", asks()), w.agent("b", "pay")}}
	}
	in := harness.Prompt("the task")
	in.RunID = "root"
	res, err := build().Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	if len(res.Waits) != 1 || res.Waits[0].Run != "root/0" {
		t.Fatalf("waits %+v", res.Waits)
	}
	if w.count("pay") != 1 || w.count("send") != 0 {
		t.Fatalf("pay %d send %d", w.count("pay"), w.count("send"))
	}
	if b := fold(t, j, "root/1"); !b.OK() {
		t.Fatalf("the free branch did not finish: %+v", b)
	}
	if got := res.Spent().ModelCalls; got != 3 {
		t.Fatalf("a suspended pattern reports what it has spent so far, got %d calls", got)
	}
	paid := w.billed()
	res, err = build().Run(ctx, harness.Resume("root", harness.Signal{Wait: res.Waits[0].ID, Value: approval.Approved, By: "alice"}))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if w.count("pay") != 1 || w.count("send") != 1 {
		t.Fatalf("pay %d send %d", w.count("pay"), w.count("send"))
	}
	if w.billed() != paid+1 {
		t.Fatalf("resuming paid for %d calls, want the waiting branch's one", w.billed()-paid)
	}
	for _, want := range []string{"## a", "send ok", "## b", "pay ok"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("missing %q in %q", want, res.Output)
		}
	}
}

// With an approval in each branch, a signal is recorded at once and nothing moves
// until the last one. Every wait of a batch resolves before the batch goes on.
func TestEachBranchOwnsItsWait(t *testing.T) {
	ctx := context.Background()
	j := newJournal()
	w := newWorld()
	build := func() *multi.Parallel {
		return &multi.Parallel{Journal: j, Branches: []harness.Runner{w.agent("a", "one", asks()), w.agent("b", "two", asks())}}
	}
	in := harness.Prompt("the task")
	in.RunID = "root"
	res, err := build().Run(ctx, in)
	if err != nil || len(res.Waits) != 2 {
		t.Fatalf("%v %+v", err, res)
	}
	first, second := res.Waits[0], res.Waits[1]
	res, err = build().Run(ctx, harness.Resume("root", harness.Signal{Wait: first.ID, Value: approval.Approved}))
	if err != nil || res.State != harness.StateSuspended || len(res.Waits) != 1 || res.Waits[0].ID != second.ID {
		t.Fatalf("%v %+v", err, res)
	}
	if w.count("one")+w.count("two") != 0 {
		t.Fatal("a branch moved before every wait was answered")
	}
	res, err = build().Run(ctx, harness.Resume("root", harness.Signal{Wait: second.ID, Value: "no", By: "bob"}))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if w.count("one") != 1 || w.count("two") != 0 {
		t.Fatalf("one %d two %d: the answers crossed branches", w.count("one"), w.count("two"))
	}
}

func TestASignalThatMatchesNothingIsRefusedAtTheRoot(t *testing.T) {
	ctx := context.Background()
	j := newJournal()
	w := newWorld()
	build := func() *multi.Parallel {
		return &multi.Parallel{Journal: j, Branches: []harness.Runner{w.agent("a", "one", asks()), w.agent("b", "two")}}
	}
	in := harness.Prompt("the task")
	in.RunID = "root"
	if _, err := build().Run(ctx, in); err != nil {
		t.Fatal(err)
	}
	before := len(fold(t, j, "root").Events)
	_, err := build().Run(ctx, harness.Resume("root", harness.Signal{Wait: "w_forged", Value: "yes"}))
	if !errors.Is(err, harness.ErrStaleSignal) {
		t.Fatalf("got %v", err)
	}
	if after := len(fold(t, j, "root").Events); after != before {
		t.Fatalf("a rejected signal wrote %d events", after-before)
	}
}

// A branch's ID comes from its position, whichever goroutine ran first.
func TestBranchIdentityDoesNotDependOnScheduling(t *testing.T) {
	ctx := context.Background()
	for i := range 30 {
		j := newJournal()
		w := newWorld()
		gate := make(chan struct{})
		late := model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
			<-gate
			return w.provider("late", "").Complete(ctx, req)
		})
		first := &harness.Agent{Name: "first", Provider: late}
		second := &harness.Agent{Name: "second", Provider: model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
			defer close(gate)
			return w.provider("early", "").Complete(ctx, req)
		})}
		in := harness.Prompt("x")
		in.RunID = fmt.Sprintf("root-%d", i)
		p := &multi.Parallel{Journal: j, Branches: []harness.Runner{first, second}}
		if _, err := p.Run(ctx, in); err != nil {
			t.Fatal(err)
		}
		if got := fold(t, j, in.RunID+"/0").Agent; got != "first" {
			t.Fatalf("branch 0 is %s", got)
		}
		if got := fold(t, j, in.RunID+"/1").Agent; got != "second" {
			t.Fatalf("branch 1 is %s", got)
		}
	}
}

// Reordering branches between a crash and a restart would hand one agent another
// agent's journal. The pattern's parts are pinned, so the run fails instead.
func TestChangedBranchesFailARunInFlight(t *testing.T) {
	ctx := context.Background()
	j := newJournal()
	w := newWorld()
	build := func(jj harness.Journal, order ...string) *multi.Parallel {
		var branches []harness.Runner
		for _, name := range order {
			branches = append(branches, w.agent(name, "pay_"+name))
		}
		return &multi.Parallel{Journal: jj, Branches: branches}
	}
	in := harness.Prompt("x")
	in.RunID = "root"
	_, err := build(kill(j, "root/1", 5, true), "a", "b").Run(ctx, in) // after branch b's tool.started
	if !errors.Is(err, errDead) {
		t.Fatal(err)
	}
	before := map[string]int{"pay_a": w.count("pay_a"), "pay_b": w.count("pay_b")}
	res, err := build(j, "b", "a").Run(ctx, harness.Resume("root"))
	if err == nil || res == nil || res.State != harness.StateFailed {
		t.Fatalf("%v %+v", err, res)
	}
	for name, n := range before {
		if w.count(name) != n {
			t.Fatalf("%s ran again under a changed pattern", name)
		}
	}
}

// blocking is a once-only tool that reports when it has started and holds until told.
func blocking(w *world, name string, started, release chan struct{}) tool.Tool {
	var once sync.Once
	return tool.Func(tool.Spec{Name: name, Effect: tool.EffectOnce}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		w.effect(name, tool.IdempotencyKey(ctx))
		once.Do(func() { close(started) })
		<-release
		return name + " ok", nil
	})
}

// A worker that went quiet comes back after another worker took the tree over. Its
// effect stands, it records nothing, and nothing is repeated.
func TestZombieWorkerThroughParallelRepeatsNothing(t *testing.T) {
	j := newJournal()
	w := newWorld()
	started, release, freeDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	build := func() *multi.Parallel {
		a := &harness.Agent{Name: "a", Provider: w.provider("a", "pay"), Tools: tool.NewRegistry(blocking(w, "pay", started, release))}
		return &multi.Parallel{Journal: j, Branches: []harness.Runner{a, w.agent("b", "other")}}
	}
	in := harness.Prompt("x")
	in.RunID = "root"
	ctx := harness.WithEvents(context.Background(), func(e harness.Event) {
		if e.Kind == harness.KindRunEnded && e.RunID == "root/1" {
			close(freeDone)
		}
	})
	zombie := make(chan error, 1)
	go func() {
		_, err := build().Run(ctx, in)
		zombie <- err
	}()
	<-started
	<-freeDone
	res, err := build().Run(context.Background(), harness.Resume("root"))
	if err != nil || res.State != harness.StateSuspended || res.Waits[0].Kind != harness.WaitUnknownOutcome {
		t.Fatalf("%v %+v", err, res)
	}
	close(release)
	if err := <-zombie; !errors.Is(err, harness.ErrFenced) {
		t.Fatalf("the zombie must be fenced, got %v", err)
	}
	res, err = build().Run(context.Background(), harness.Resume("root", harness.Signal{Wait: res.Waits[0].ID, Value: "pay ok"}))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if w.count("pay") != 1 || w.count("other") != 1 {
		t.Fatalf("pay %d other %d", w.count("pay"), w.count("other"))
	}
}

// pauseOnLoad holds a worker between reading a run and claiming it, which is the
// window in which another worker can move that run on.
type pauseOnLoad struct {
	harness.Journal
	run             string
	reached, resume chan struct{}
	once            sync.Once
}

func (p *pauseOnLoad) Load(ctx context.Context, id string) ([]harness.Event, error) {
	evs, err := p.Journal.Load(ctx, id)
	if id == p.run {
		p.once.Do(func() {
			close(p.reached)
			<-p.resume
		})
	}
	return evs, err
}

// A tree has one fence per run, so ownership can split. A second worker claims branch
// a while the first, still alive, wins branch b. Each now holds what the other needs,
// and both stop with ErrFenced. Nothing is repeated and nothing is lost, but nobody
// owns the tree until the next Run, which is what a sweep is for: the stall lasts at
// most one Lease.
func TestTwoLiveWorkersCanFenceEachOtherAndTheNextRunConverges(t *testing.T) {
	store := newSweep()
	w := newWorld()
	startedA, startedB, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	build := func(j harness.Journal) *multi.Parallel {
		return &multi.Parallel{Journal: j, Branches: []harness.Runner{
			&harness.Agent{Name: "a", Provider: w.provider("a", "pay_a"), Tools: tool.NewRegistry(blocking(w, "pay_a", startedA, release))},
			&harness.Agent{Name: "b", Provider: w.provider("b", "pay_b"), Tools: tool.NewRegistry(blocking(w, "pay_b", startedB, release))},
		}}
	}
	in := harness.Prompt("x")
	in.RunID = "root"
	first := make(chan error, 1)
	go func() {
		_, err := build(store).Run(context.Background(), in)
		first <- err
	}()
	<-startedA
	<-startedB

	paused := &pauseOnLoad{Journal: store, run: "root/1", reached: make(chan struct{}), resume: make(chan struct{})}
	second := make(chan error, 1)
	go func() {
		_, err := build(paused).Run(context.Background(), harness.Resume("root"))
		second <- err
	}()
	<-paused.reached
	for fold(t, store.jsonJournal, "root/0").Claims == 0 {
		runtime.Gosched()
	}
	close(release)
	if err := <-first; !errors.Is(err, harness.ErrFenced) {
		t.Fatalf("first worker: %v", err)
	}
	close(paused.resume)
	if err := <-second; !errors.Is(err, harness.ErrFenced) {
		t.Fatalf("second worker: %v", err)
	}

	if due := store.due(time.Now()); len(due) != 0 {
		t.Fatalf("due inside the lease: %v", due)
	}
	due := store.due(time.Now().Add(harness.DefaultLease + time.Minute))
	if !slices.Equal(due, []string{"root"}) {
		t.Fatalf("the sweep did not find the orphaned tree: %v", due)
	}
	res := drive(t, w, func() harness.Runner { return build(store) }, due[0])
	if !res.OK() {
		t.Fatalf("%+v", res)
	}
	if w.count("pay_a") != 1 || w.count("pay_b") != 1 {
		t.Fatalf("pay_a %d pay_b %d", w.count("pay_a"), w.count("pay_b"))
	}
}

func TestDrainThroughParallelLeavesItResumable(t *testing.T) {
	j := newJournal()
	w := newWorld()
	ctx, drain := harness.WithDrain(context.Background())
	a := &harness.Agent{Name: "a", Provider: w.provider("a", "pay"), Tools: tool.NewRegistry(tool.Func(tool.Spec{Name: "pay", Effect: tool.EffectIdempotent},
		func(ctx context.Context, _ json.RawMessage) (string, error) {
			drain()
			return "pay ok", nil
		}))}
	p := &multi.Parallel{Journal: j, Branches: []harness.Runner{a}}
	in := harness.Prompt("x")
	in.RunID = "root"
	if _, err := p.Run(ctx, in); !errors.Is(err, harness.ErrDrained) {
		t.Fatalf("got %v", err)
	}
	if s := fold(t, j, "root").State; s.Terminal() {
		t.Fatalf("a drain ended the pattern: %s", s)
	}
	res, err := p.Run(context.Background(), harness.Resume("root"))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
}
