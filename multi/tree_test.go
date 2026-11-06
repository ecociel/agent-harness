package multi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/approval"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/multi"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func say(text string) model.Provider {
	return fake.NewFunc(func(model.Request) (model.Response, error) { return fake.Text(text), nil })
}

func bot(name, text string) *harness.Agent { return &harness.Agent{Name: name, Provider: say(text)} }

func broken(name string) *harness.Agent {
	return &harness.Agent{Name: name, Provider: fake.NewFunc(func(model.Request) (model.Response, error) {
		return model.Response{}, errors.New("provider down")
	})}
}

func TestParallelFailsWhenABranchFails(t *testing.T) {
	p := &multi.Parallel{Branches: []harness.Runner{bot("ok", "fine"), broken("bad")}}
	res, err := p.Run(context.Background(), harness.Prompt("x"))
	if err == nil || res.OK() || !strings.Contains(res.Detail, "bad") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	p.AllowPartial = true
	res, _ = p.Run(context.Background(), harness.Prompt("x"))
	if !res.OK() || !strings.Contains(res.Output, "fine") {
		t.Fatalf("partial: %+v", res)
	}
}

func TestSequentialStopsWithTheStepsReason(t *testing.T) {
	limited := &harness.Agent{Name: "limited", MaxTurns: 1, Provider: fake.NewFunc(func(model.Request) (model.Response, error) {
		return fake.Call("noop", nil), nil
	}), Tools: tool.NewRegistry(tool.Func(tool.Spec{Name: "noop", Effect: tool.EffectRead},
		func(context.Context, json.RawMessage) (string, error) { return "x", nil }))}
	never := 0
	after := &harness.Agent{Name: "after", Provider: fake.NewFunc(func(model.Request) (model.Response, error) {
		never++
		return fake.Text("x"), nil
	})}
	p := &multi.Sequential{Steps: []harness.Runner{limited, after}}
	res, err := p.Run(context.Background(), harness.Prompt("x"))
	if err != nil || res.Stop != harness.StopMaxTurns || res.State != harness.StateStopped || never != 0 {
		t.Fatalf("%v %+v never=%d", err, res, never)
	}
}

// A Runner can hand back a run that did not complete and gives no reason. It must not
// read as an answer.
func TestAChildThatStoppedWithNoReasonDoesNotCompleteThePattern(t *testing.T) {
	odd := harness.RunnerFunc(func(context.Context, harness.Input) (*harness.Run, error) {
		return &harness.Run{State: harness.StateStopped, Output: "partial"}, nil
	})
	res, err := (&multi.Sequential{Steps: []harness.Runner{odd}}).Run(context.Background(), harness.Prompt("x"))
	if err == nil || res.OK() || res.State != harness.StateFailed {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestPatternsNeedNoJournalForOneLine(t *testing.T) {
	p := &multi.Sequential{Steps: []harness.Runner{bot("a", "1"), bot("b", "2")}}
	res, err := p.Run(context.Background(), harness.Prompt("x"))
	if err != nil || res.Output != "2" || res.Spent().ModelCalls != 2 {
		t.Fatalf("%v %+v", err, res)
	}
	if want := []model.Role{model.RoleUser, model.RoleAssistant}; len(res.Messages) != 2 || res.Messages[0].Role != want[0] || res.Messages[1].Content != "2" {
		t.Fatalf("the scripted turns leaked into the transcript: %+v", res.Messages)
	}
}

func TestASessionOverAPatternKeepsAPlainHistory(t *testing.T) {
	j := newJournal()
	p := &multi.Parallel{Journal: j, Branches: []harness.Runner{bot("a", "1")}}
	s := harness.NewSession(p)
	if _, err := s.Send(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Send(context.Background(), "two")
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	for _, m := range res.Messages {
		if m.Role == model.RoleTool || len(m.ToolCalls) > 0 {
			t.Fatalf("history holds the lead's scripted turn: %+v", m)
		}
	}
}

func TestSubagentDepthCap(t *testing.T) {
	self := &harness.Agent{Name: "self", MaxTurns: 3}
	self.Provider = fake.NewFunc(func(req model.Request) (model.Response, error) {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == model.RoleTool {
			return fake.Text("tool said: " + last.Content), nil
		}
		return fake.Call("again", map[string]string{"task": "recurse"}), nil
	})
	withTool, err := multi.WithSubagents(self, multi.Subagent{Name: "again", Agent: harness.RunnerFunc(
		func(ctx context.Context, in harness.Input) (*harness.Run, error) { return self.Run(ctx, in) }), MaxDepth: 2})
	if err != nil {
		t.Fatal(err)
	}
	self.Tools = withTool.Tools
	res, err := self.Run(context.Background(), harness.Prompt("go"))
	if err != nil || !res.OK() {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if !strings.Contains(res.Output, "exceeds limit 2") {
		t.Fatalf("depth cap not reported: %q", res.Output)
	}
}

func TestBranchesAreChildrenOfThePattern(t *testing.T) {
	var mu sync.Mutex
	var evs []harness.Event
	ctx := harness.WithEvents(context.Background(), func(e harness.Event) { mu.Lock(); evs = append(evs, e); mu.Unlock() })
	p := &multi.Parallel{Name: "fan", Branches: []harness.Runner{bot("a", "1"), bot("b", "2")}}
	in := harness.Prompt("x")
	in.RunID = "root"
	if _, err := p.Run(ctx, in); err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, e := range evs {
		if e.Kind == harness.KindRunStarted && e.RunID != "root" {
			starts++
			if e.ParentRun != "root" || e.RootRun != "root" || e.Depth != 1 {
				t.Fatalf("branch run not nested: %+v", e)
			}
		}
	}
	if starts != 2 {
		t.Fatalf("starts=%d", starts)
	}
}

// A branch that failed in the provider fails for good. A branch whose worker stopped
// has not failed, and treating the two alike would end a run a restart could save.
func TestAStoppedBranchIsNotAFailedBranch(t *testing.T) {
	ctx := context.Background()
	j := newJournal()
	w := newWorld()
	build := func(jj harness.Journal) *multi.Parallel {
		return &multi.Parallel{Journal: jj, AllowPartial: true, Branches: []harness.Runner{w.agent("a", "pay_a"), w.agent("b", "pay_b")}}
	}
	in := harness.Prompt("x")
	in.RunID = "root"
	blip := &oneFailure{Journal: j, run: "root/1"}
	if res, err := build(blip).Run(ctx, in); err == nil {
		t.Fatalf("AllowPartial swallowed a stopped branch: %+v", res)
	}
	res := drive(t, w, func() harness.Runner { return build(j) }, "root")
	if !res.OK() || !strings.Contains(res.Output, "## b") || !strings.Contains(res.Output, "pay_b ok") {
		t.Fatalf("%+v", res)
	}
}

type oneFailure struct {
	harness.Journal
	run  string
	mu   sync.Mutex
	done bool
}

func (o *oneFailure) Append(ctx context.Context, id string, evs []harness.Event) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if id == o.run && evs[0].Kind == harness.KindModelDone && !o.done {
		o.done = true
		return errDead
	}
	return o.Journal.Append(ctx, id, evs)
}

type damaged struct {
	harness.Journal
	root string
}

func (d damaged) Load(ctx context.Context, id string) ([]harness.Event, error) {
	if id != d.root {
		return []harness.Event{{Seq: 7, Kind: "nonsense"}}, nil
	}
	return d.Journal.Load(ctx, id)
}

// A child whose journal cannot be read has not failed, and the lead must not tell its
// model so: the model would delegate again, to a new child, under a new key.
func TestADamagedChildJournalStopsTheLeadInsteadOfReachingItsModel(t *testing.T) {
	w := newWorld()
	l := lead(w, damaged{newJournal(), "root"}, w.agent("child", "pay"))
	in := harness.Prompt("x")
	in.RunID = "root"
	res, err := l.Run(context.Background(), in)
	if !errors.Is(err, harness.ErrDrained) || !errors.Is(err, harness.ErrIllegalTransition) {
		t.Fatalf("%v %+v", err, res)
	}
	if w.billed() != 1 {
		t.Fatalf("the lead's model was asked again: %d calls", w.billed())
	}
}

type slowStart struct{ d time.Duration }

func (slowStart) Name() string { return "slow" }
func (s slowStart) BeforeRun(_ context.Context, r harness.Run) error {
	if r.Claims == 1 {
		time.Sleep(s.d)
	}
	return nil
}

// The parent accepts an answer an instant before the child's clock passes the
// expiry. The child refuses it. That must read as an expired wait, not as a stopped
// worker, or every restart meets the same refusal and the tree never moves again.
func TestASignalCrossingAChildsExpiryDoesNotWedgeTheTree(t *testing.T) {
	ctx := context.Background()
	j := newJournal()
	w := newWorld()
	build := func() *harness.Agent {
		l := lead(w, j, w.agent("child", "send", &approval.Policy{Threshold: tool.RiskWrite, Ask: true, TTL: 40 * time.Millisecond}))
		l.Hooks = []harness.Hook{slowStart{120 * time.Millisecond}}
		return l
	}
	in := harness.Prompt("x")
	in.RunID = "root"
	res, err := build().Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	old := res.Waits[0].ID
	res, err = build().Run(ctx, harness.Resume("root", harness.Signal{Wait: old, Value: approval.Approved}))
	if err != nil {
		t.Fatalf("the tree wedged: %v", err)
	}
	if res.State != harness.StateSuspended || res.Waits[0].ID == old {
		t.Fatalf("want a fresh ask, got %+v", res)
	}
	if w.count("send") != 0 {
		t.Fatal("an expired approval ran the tool")
	}
	res, err = build().Run(ctx, harness.Resume("root", harness.Signal{Wait: res.Waits[0].ID, Value: approval.Approved}))
	if err != nil || !res.OK() || w.count("send") != 1 {
		t.Fatalf("%v %+v send=%d", err, res, w.count("send"))
	}
}

// Two answers arrive together and one has just expired below. The expired one must
// not take the good one down with it.
func TestAStaleAnswerDoesNotSwallowAFreshOne(t *testing.T) {
	ctx := context.Background()
	j := newJournal()
	w := newWorld()
	build := func() *harness.Agent {
		fan := &multi.Parallel{Name: "fan", Branches: []harness.Runner{
			w.agent("a", "short", &approval.Policy{Threshold: tool.RiskWrite, Ask: true, TTL: 40 * time.Millisecond}),
			w.agent("b", "long", &approval.Policy{Threshold: tool.RiskWrite, Ask: true, TTL: time.Hour}),
		}}
		l := lead(w, j, fan)
		l.Hooks = []harness.Hook{slowStart{120 * time.Millisecond}}
		return l
	}
	in := harness.Prompt("x")
	in.RunID = "root"
	res, err := build().Run(ctx, in)
	if err != nil || len(res.Waits) != 2 {
		t.Fatalf("%v %+v", err, res)
	}
	res, err = build().Run(ctx, harness.Resume("root", answer(w, res.Waits)...))
	if err != nil {
		t.Fatalf("the tree wedged: %v", err)
	}
	if res.State != harness.StateSuspended || len(res.Waits) != 1 || res.Waits[0].Run != w.lastRun("short", j) {
		t.Fatalf("only the expired ask should come back, got %+v", res.Waits)
	}
	if w.count("short") != 0 {
		t.Fatal("an expired approval ran the tool")
	}
	if w.count("long") != 1 {
		t.Fatalf("the good answer was swallowed: long ran %d times", w.count("long"))
	}
	res, err = build().Run(ctx, harness.Resume("root", answer(w, res.Waits)...))
	if err != nil || !res.OK() || w.count("short") != 1 || w.count("long") != 1 {
		t.Fatalf("%v %+v short=%d long=%d", err, res, w.count("short"), w.count("long"))
	}
}

// Input.Cancel ends a parked child too. Its hooks run in the process that cancelled,
// and a late answer finds nothing to move.
func TestCancelEndsAParkedChild(t *testing.T) {
	ctx := context.Background()
	j := newJournal()
	w := newWorld()
	closed := &closings{}
	build := func() *multi.Parallel {
		return &multi.Parallel{Journal: j, Branches: []harness.Runner{w.agent("a", "send", asks(), closed)}}
	}
	in := harness.Prompt("x")
	in.RunID = "root"
	res, err := build().Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	wait := res.Waits[0]
	res, err = build().Run(ctx, harness.Input{RunID: "root", Cancel: "operator"})
	if err != nil || res.Stop != harness.StopCancelled || !res.State.Terminal() {
		t.Fatalf("%v %+v", err, res)
	}
	child := fold(t, j, "root/0")
	if child.Stop != harness.StopCancelled || !child.State.Terminal() || child.Detail != "operator" || len(child.Waits) != 0 {
		t.Fatalf("child %s %s %q waits %v", child.State, child.Stop, child.Detail, child.Waits)
	}
	if closed.count() != 1 {
		t.Fatalf("the child's AfterRun ran %d times", closed.count())
	}
	if _, err = build().Run(ctx, harness.Resume("root", harness.Signal{Wait: wait.ID, Value: approval.Approved})); err != nil {
		t.Fatal(err)
	}
	if w.count("send") != 0 {
		t.Fatal("a cancelled tree ran a tool")
	}
}

// Input.Cancel reaches a branch another worker is running. The tool in flight
// finishes, because nothing can recall it, and its result is not recorded. The branch
// pays for no further model call and starts no further tool.
func TestCancelStopsABranchThatIsAlreadyRunning(t *testing.T) {
	j := newJournal()
	w := newWorld()
	started, release := make(chan struct{}), make(chan struct{})
	build := func() *multi.Parallel {
		a := &harness.Agent{Name: "a", Provider: w.turns("a", "pay", "next"),
			Tools: tool.NewRegistry(blocking(w, "pay", started, release), w.tool("next", tool.Spec{Effect: tool.EffectOnce}))}
		return &multi.Parallel{Journal: j, Branches: []harness.Runner{a}}
	}
	in := harness.Prompt("x")
	in.RunID = "root"
	worker := make(chan error, 1)
	go func() {
		_, err := build().Run(context.Background(), in)
		worker <- err
	}()
	<-started
	res, err := build().Run(context.Background(), harness.Input{RunID: "root", Cancel: "stop now"})
	if err != nil || res.Stop != harness.StopCancelled {
		t.Fatalf("%v %+v", err, res)
	}
	if b := fold(t, j, "root/0"); b.Stop != harness.StopCancelled || !b.State.Terminal() {
		t.Fatalf("the cancel did not reach the branch: %s %s", b.State, b.Stop)
	}
	paidAtCancel := w.billed()
	close(release)
	if err := <-worker; !errors.Is(err, harness.ErrFenced) {
		t.Fatalf("got %v", err)
	}
	if b := fold(t, j, "root/0"); b.Stop != harness.StopCancelled || b.ToolCalls != 0 {
		t.Fatalf("the fenced worker moved the branch: %+v", b)
	}
	if w.billed() != paidAtCancel {
		t.Fatalf("billed %d after the cancel", w.billed()-paidAtCancel)
	}
	if w.count("next") != 0 {
		t.Fatal("a cancelled branch started another tool")
	}
}

// background is a lead that starts one job, collects it and answers.
func background(w *world, j harness.Journal, bgs *[]*multi.Background, mu *sync.Mutex) *harness.Agent {
	child := w.agent("worker", "send")
	b := multi.NewBackground(multi.Subagent{Name: "worker", Agent: child})
	mu.Lock()
	*bgs = append(*bgs, b)
	mu.Unlock()
	p := model.ProviderFunc(func(_ context.Context, req model.Request) iter.Seq2[model.Event, error] {
		w.mu.Lock()
		w.paid++
		w.mu.Unlock()
		reply := model.Response{Usage: model.Usage{Input: 10, Output: 1}}
		last := req.Messages[len(req.Messages)-1]
		switch {
		case last.Role != model.RoleTool:
			reply.ToolCalls = []model.ToolCall{{ID: "s", Name: "start_worker", Arguments: json.RawMessage(`{"task":"do it"}`)}}
		case last.Name == "start_worker":
			var job struct{ Job string }
			if err := json.Unmarshal([]byte(last.Content), &job); err != nil {
				return model.Fail(err)
			}
			args, _ := json.Marshal(map[string]any{"id": job.Job, "wait": true})
			reply.ToolCalls = []model.ToolCall{{ID: "c", Name: "collect", Arguments: args}}
		default:
			reply.Text = "job said " + last.Content
		}
		return model.Stream(reply)
	})
	return &harness.Agent{Name: "lead", Journal: j, Provider: p, Tools: tool.NewRegistry(b.Tools()...)}
}

func TestCrashSweepThroughABackgroundJob(t *testing.T) {
	var mu sync.Mutex
	var bgs []*multi.Background
	sweep(t, scenario{id: "root", tools: []string{"send"},
		settle: func() {
			mu.Lock()
			defer mu.Unlock()
			for _, b := range bgs {
				b.Wait()
			}
		},
		build: func(w *world, j harness.Journal) harness.Runner { return background(w, j, &bgs, &mu) }})
}

func TestCollectingAJobThatNeverExistedIsAToolError(t *testing.T) {
	j := newJournal()
	b := multi.NewBackground(multi.Subagent{Name: "worker", Agent: bot("w", "x")})
	p := fake.Script(fake.Call("collect", map[string]any{"id": "worker/invented", "wait": true}), fake.Text("sorry"))
	a := &harness.Agent{Journal: j, Provider: p, Tools: tool.NewRegistry(b.Tools()...)}
	in := harness.Prompt("x")
	in.RunID = "root"
	res, err := a.Run(context.Background(), in)
	if err != nil || !res.OK() || !strings.Contains(res.Messages[2].Content, "no job") {
		t.Fatalf("%v %+v", err, res)
	}
}

// lastRun names the run that holds a call to the given tool.
func (w *world) lastRun(toolName string, j *jsonJournal) string {
	for _, id := range j.ids() {
		evs, _ := j.Load(context.Background(), id)
		for _, e := range evs {
			if e.Kind == harness.KindToolGated && e.Tool == toolName {
				return id
			}
		}
	}
	return ""
}

const raw = "756.1234.5678.97"

type scrubber struct{}

func (scrubber) Name() string { return "scrubber" }
func (scrubber) Redact(_ context.Context, _ harness.Kind, s string) (string, error) {
	return strings.ReplaceAll(s, raw, "[AHV]"), nil
}

type spy struct {
	mu   sync.Mutex
	seen strings.Builder
}

func (*spy) Name() string { return "spy" }
func (s *spy) note(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen.WriteString(text + "\n")
}
func (s *spy) WrapTool(_ harness.Run, next harness.ToolFunc) harness.ToolFunc {
	return func(ctx context.Context, c harness.Call) (string, error) {
		s.note(string(c.Arguments))
		out, err := next(ctx, c)
		s.note(fmt.Sprint(out, err))
		return out, err
	}
}
func (s *spy) WrapModel(_ harness.Run, next model.Provider) model.Provider {
	return model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
		b, _ := json.Marshal(req)
		s.note(string(b))
		return func(yield func(model.Event, error) bool) {
			for e, err := range next.Complete(ctx, req) {
				s.note(fmt.Sprintf("%+v %v", e, err))
				if !yield(e, err) {
					return
				}
			}
		}
	})
}

// One Redactor on the pattern covers the input it stores, every branch, and every
// hook inside them, though no branch names a redactor.
func TestARedactorOnThePatternCoversTheTree(t *testing.T) {
	var mu sync.Mutex
	var sunk []harness.Event
	ctx := harness.WithEvents(context.Background(), func(e harness.Event) { mu.Lock(); sunk = append(sunk, e); mu.Unlock() })
	j := newJournal()
	s := &spy{}
	leak := tool.Func(tool.Spec{Name: "leak"}, func(context.Context, json.RawMessage) (string, error) { return "customer " + raw, nil })
	branch := func(name string) *harness.Agent {
		return &harness.Agent{Name: name, Hooks: []harness.Hook{s}, Tools: tool.NewRegistry(leak),
			Provider: fake.NewFunc(func(req model.Request) (model.Response, error) {
				if req.Messages[len(req.Messages)-1].Role == model.RoleTool {
					return fake.Text("found " + raw), nil
				}
				return fake.Call("leak", map[string]string{"id": raw}), nil
			})}
	}
	p := &multi.Parallel{Journal: j, Hooks: []harness.Hook{scrubber{}}, Branches: []harness.Runner{branch("a"), branch("b")}}
	in := harness.Prompt("my id is " + raw)
	in.RunID = "root"
	res, err := p.Run(ctx, in)
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if strings.Contains(res.Output, raw) {
		t.Errorf("output %q", res.Output)
	}
	if strings.Contains(j.dump(), raw) {
		t.Error("the journal holds the raw value")
	}
	if strings.Contains(s.seen.String(), raw) {
		t.Errorf("a WrapTool or WrapModel hook saw the raw value:\n%s", s.seen.String())
	}
	if !strings.Contains(s.seen.String(), "[AHV]") {
		t.Error("the spy saw nothing, so it proves nothing")
	}
	mu.Lock()
	defer mu.Unlock()
	b, _ := json.Marshal(sunk)
	if strings.Contains(string(b), raw) {
		t.Error("a sink saw the raw value")
	}
}

// Branch a parks for a day. Then branch b dies. A parked child's Wake is in the future,
// and that must not read as a sign of life, or the crash stays hidden for a day.
func TestSweepFindsACrashedBranchBesideAParkedOne(t *testing.T) {
	j := newSweep()
	w := newWorld()
	parked := make(chan struct{})
	var once sync.Once
	ctx := harness.WithEvents(context.Background(), func(e harness.Event) {
		if e.RunID == "root/0" && e.To == harness.StateSuspended {
			once.Do(func() { close(parked) })
		}
	})
	second := w.agent("b", "pay")
	free := second.Provider
	second.Provider = model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
		<-parked
		return free.Complete(ctx, req)
	})
	p := &multi.Parallel{Journal: kill(j, "root/1", 3, false), Branches: []harness.Runner{
		w.agent("a", "send", &approval.Policy{Threshold: tool.RiskWrite, Ask: true, TTL: 24 * time.Hour}), second}}
	in := harness.Prompt("x")
	in.RunID = "root"
	if _, err := p.Run(ctx, in); !errors.Is(err, errDead) {
		t.Fatalf("got %v", err)
	}
	late := time.Now().Add(harness.DefaultLease + time.Minute)
	if r := j.rows["root/0"]; r.state != harness.StateSuspended || !r.wake.After(late) {
		t.Fatalf("branch a should be parked past the sweep: %+v", r)
	}
	if due := j.due(late); len(due) != 1 || due[0] != "root" {
		t.Fatalf("a parked sibling hid a crashed branch: %v", due)
	}
}

// crashTree starts a tree over a sweep journal and kills it at one append of one run.
func crashTree(t *testing.T, j *sweepJournal, run string, at int, build func(harness.Journal) harness.Runner) {
	t.Helper()
	in := harness.Prompt("x")
	in.RunID = "root"
	if _, err := build(kill(j, run, at, false)).Run(context.Background(), in); !errors.Is(err, errDead) {
		t.Fatalf("got %v", err)
	}
}

// The consumer's Lease on a pattern decides when a crashed branch is found. The
// control is the same tree on the default.
func TestAPatternsLeaseDecidesWhenACrashedBranchIsFound(t *testing.T) {
	run := func(lease time.Duration) []string {
		j := newSweep()
		w := newWorld()
		crashTree(t, j, "root/0", 3, func(jj harness.Journal) harness.Runner {
			b := w.agent("b", "pay")
			b.Lease = time.Minute
			return &multi.Parallel{Journal: jj, Lease: lease, Branches: []harness.Runner{b}}
		})
		return j.due(time.Now().Add(2 * time.Minute))
	}
	if due := run(time.Minute); len(due) != 1 || due[0] != "root" {
		t.Fatalf("the crash was not found on the pattern's lease: %v", due)
	}
	if due := run(0); len(due) != 0 {
		t.Fatalf("a pattern with no Lease takes the default: %v", due)
	}
}

func TestASequentialsLeaseDecidesToo(t *testing.T) {
	j := newSweep()
	w := newWorld()
	crashTree(t, j, "root/0", 3, func(jj harness.Journal) harness.Runner {
		b := w.agent("b", "pay")
		b.Lease = time.Minute
		return &multi.Sequential{Journal: jj, Lease: time.Minute, Steps: []harness.Runner{b}}
	})
	if due := j.due(time.Now().Add(2 * time.Minute)); len(due) != 1 {
		t.Fatalf("due %v", due)
	}
}

// A pattern's Lease is a floor. Its branch is working inside a much longer lease, so
// the root's Wake passing means nothing yet.
func TestAShortPatternLeaseDoesNotFireOverAWorkingBranch(t *testing.T) {
	j := newSweep()
	w := newWorld()
	started, release := make(chan struct{}), make(chan struct{})
	slow := &harness.Agent{Name: "slow", Lease: 2 * time.Hour, Provider: w.provider("slow", "pay"),
		Tools: tool.NewRegistry(blocking(w, "pay", started, release))}
	p := &multi.Parallel{Journal: j, Lease: time.Minute, Branches: []harness.Runner{slow}}
	in := harness.Prompt("x")
	in.RunID = "root"
	done := make(chan error, 1)
	go func() {
		_, err := p.Run(context.Background(), in)
		done <- err
	}()
	<-started
	if due := j.due(time.Now().Add(time.Hour)); len(due) != 0 {
		t.Errorf("a working branch was swept: %v", due)
	}
	if due := j.due(time.Now().Add(3 * time.Hour)); len(due) != 1 {
		t.Errorf("a tree whose branch went quiet must be found: %v", due)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if due := j.due(time.Now().Add(3 * time.Hour)); len(due) != 0 {
		t.Fatalf("a finished tree stays due: %v", due)
	}
}

// A nested pattern does no work, so its row must not read as a sign of life. On its
// default lease it would hide this crash for ten minutes.
func TestANestedPatternCannotHideACrash(t *testing.T) {
	j := newSweep()
	w := newWorld()
	crashTree(t, j, "root/0/0", 3, func(jj harness.Journal) harness.Runner {
		b := w.agent("b", "pay")
		b.Lease = time.Minute
		return &multi.Sequential{Journal: jj, Lease: time.Minute, Steps: []harness.Runner{
			&multi.Parallel{Branches: []harness.Runner{b}},
		}}
	})
	if r := j.rows["root/0"]; r.state != harness.StateBody || !r.wake.IsZero() {
		t.Fatalf("a nested pattern should have no Wake: %+v", r)
	}
	if due := j.due(time.Now().Add(2 * time.Minute)); len(due) != 1 || due[0] != "root" {
		t.Fatalf("a nested pattern hid a crash below it: %v", due)
	}
}

type sweepRow struct {
	root, parent string
	state        harness.State
	wake         time.Time
}

type sweepJournal struct {
	*jsonJournal
	mu   sync.Mutex
	rows map[string]sweepRow
}

func newSweep() *sweepJournal {
	return &sweepJournal{jsonJournal: newJournal(), rows: map[string]sweepRow{}}
}

func (j *sweepJournal) Append(ctx context.Context, id string, evs []harness.Event) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.jsonJournal.Append(ctx, id, evs); err != nil {
		return err
	}
	last := evs[len(evs)-1]
	j.rows[id] = sweepRow{last.RootRun, last.ParentRun, last.To, last.Wake}
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
	return out
}
