package budget_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/budget"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/multi"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func used(in, out int) model.Usage { return model.Usage{Input: in, Output: out} }

func TestTokenLimitStopsSecondTurn(t *testing.T) {
	b := budget.New(budget.Limits{Tokens: 100})
	p := fake.Script(
		model.Response{ToolCalls: []model.ToolCall{{ID: "1", Name: "noop", Arguments: json.RawMessage(`{}`)}}, Usage: used(80, 30)},
		fake.Text("never"))
	noop := tool.Func(tool.Spec{Name: "noop"}, func(context.Context, json.RawMessage) (string, error) { return "", nil })
	a := &harness.Agent{Provider: p, Tools: tool.NewRegistry(noop), Hooks: []harness.Hook{b}}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != harness.StopBudget {
		t.Fatalf("stop = %s (%s)", res.Stop, res.Detail)
	}
	if b.Spent(*res).Usage.Total() != 110 {
		t.Fatalf("spent = %+v", b.Spent(*res))
	}
}

func TestRepeatDetection(t *testing.T) {
	call := model.Response{ToolCalls: []model.ToolCall{{ID: "1", Name: "noop", Arguments: json.RawMessage(`{"q":1}`)}}}
	p := fake.Script(call, call, call, fake.Text("x"))
	noop := tool.Func(tool.Spec{Name: "noop"}, func(context.Context, json.RawMessage) (string, error) { return "", nil })
	a := &harness.Agent{Provider: p, Tools: tool.NewRegistry(noop),
		Hooks: []harness.Hook{budget.New(budget.Limits{RepeatCalls: 3})}}
	res, _ := a.Run(context.Background(), harness.Prompt("x"))
	if res.Stop != harness.StopLoop || res.Turn != 3 {
		t.Fatalf("stop=%s turns=%d", res.Stop, res.Turn)
	}
}

func TestPreflightEstimate(t *testing.T) {
	b := budget.New(budget.Limits{Tokens: 5})
	a := &harness.Agent{Provider: fake.Script(fake.Text("x")), Hooks: []harness.Hook{b}}
	res, _ := a.Run(context.Background(), harness.Prompt("a long prompt that is clearly more than five tokens"))
	if res.Stop != harness.StopBudget || res.ModelCalls != 0 {
		t.Fatalf("stop=%s calls=%d", res.Stop, res.ModelCalls)
	}
}

type dying struct {
	harness.Journal
	at, n int
	after bool
}

var errDead = errors.New("process died")

func (d *dying) Append(ctx context.Context, id string, evs []harness.Event) error {
	if d.n++; d.n >= d.at {
		if d.n == d.at && d.after {
			_ = d.Journal.Append(ctx, id, evs)
		}
		return errDead
	}
	return d.Journal.Append(ctx, id, evs)
}

// asker asks for a tool until it has seen n results, and counts what it was paid.
func asker(paid *int, each model.Usage) model.Provider {
	return fake.NewFunc(func(req model.Request) (model.Response, error) {
		*paid++
		return model.Response{Usage: each, ToolCalls: []model.ToolCall{{ID: "1", Name: "noop", Arguments: json.RawMessage(fmt.Sprintf(`{"n":%d}`, len(req.Messages)))}}}, nil
	})
}

var noop = tool.Func(tool.Spec{Name: "noop", Effect: tool.EffectIdempotent},
	func(context.Context, json.RawMessage) (string, error) { return "", nil })

// The hook is built fresh by each process and holds no spend. A limit reached before
// the crash is still reached after it, with no restore step anywhere.
func TestSpendSurvivesARestartThroughAFreshHook(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	paid := 0
	build := func(jj harness.Journal) *harness.Agent {
		return &harness.Agent{Journal: jj, Provider: asker(&paid, used(40, 0)), Tools: tool.NewRegistry(noop),
			MaxTurns: 50, Hooks: []harness.Hook{budget.New(budget.Limits{Tokens: 100})}}
	}
	in := harness.Prompt("x")
	in.RunID = "r"
	// started, model x2, gated, started, done, model.started, model.done (40+40 spent)
	if _, err := build(&dying{Journal: j, at: 9}).Run(ctx, in); !errors.Is(err, errDead) {
		t.Fatalf("got %v", err)
	}
	res, err := build(j).Run(ctx, harness.Resume("r"))
	if err != nil || res.Stop != harness.StopBudget {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Usage.Input != 80 || paid != 2 || !strings.Contains(res.Detail, "remaining 20") {
		t.Fatalf("spent %d over %d calls, %q: the new process did not see the old spend", res.Usage.Input, paid, res.Detail)
	}
}

// A call a crash cut off may have been billed. A call limit counts it, because the
// journal cannot say it was not, so a crash loop cannot pay past the limit.
func TestLostCallsCountAgainstACallLimit(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	paid := 0
	build := func(jj harness.Journal) *harness.Agent {
		return &harness.Agent{Journal: jj, Provider: asker(&paid, used(1, 0)), Tools: tool.NewRegistry(noop),
			MaxTurns: 50, Hooks: []harness.Hook{budget.New(budget.Limits{ModelCalls: 3})}}
	}
	in := harness.Prompt("x")
	in.RunID = "r"
	// Die on model.done, every time: the provider bills and the journal never hears.
	for range 2 {
		n := len(events(t, j, "r"))
		if _, err := build(&dyingOn{Journal: j, kind: harness.KindModelDone}).Run(ctx, in); !errors.Is(err, errDead) {
			t.Fatalf("got %v after %d events", err, n)
		}
	}
	res, err := build(j).Run(ctx, harness.Resume("r"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != harness.StopBudget || paid != 3 {
		t.Fatalf("stop %s after %d paid calls, lost %d", res.Stop, paid, res.LostCalls)
	}
}

type dyingOn struct {
	harness.Journal
	kind harness.Kind
}

func (d *dyingOn) Append(ctx context.Context, id string, evs []harness.Event) error {
	if evs[0].Kind == d.kind {
		return errDead
	}
	return d.Journal.Append(ctx, id, evs)
}

func events(t *testing.T, j harness.Journal, id string) []harness.Event {
	t.Helper()
	evs, err := j.Load(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// ledger is what a consumer keeps: usage per tree, written inside Append, where
// (RunID, Seq) is unique, so a reply counts once whoever retries.
type ledger struct {
	harness.Journal
	mu    sync.Mutex
	trees map[string]model.Usage
}

func (l *ledger) Append(ctx context.Context, id string, evs []harness.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.Journal.Append(ctx, id, evs); err != nil {
		return err
	}
	for _, e := range evs {
		if e.Kind == harness.KindModelDone {
			l.trees[e.RootRun] = l.trees[e.RootRun].Add(e.Reply.Usage)
		}
	}
	return nil
}

func (l *ledger) pool(_ context.Context, r harness.Run) (budget.Spent, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return budget.Spent{Usage: l.trees[r.RootRun]}, nil
}

// A run that waited a week for an approval has not used a week of its wall clock.
func TestTimeSuspendedDoesNotCountAgainstTheWallLimit(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	build := func() *harness.Agent {
		p := fake.NewFunc(func(req model.Request) (model.Response, error) {
			if req.Messages[len(req.Messages)-1].Role == model.RoleTool {
				return fake.Text("done"), nil
			}
			return fake.Call("noop", nil), nil
		})
		return &harness.Agent{Journal: j, Provider: p, Tools: tool.NewRegistry(noop),
			Hooks: []harness.Hook{budget.New(budget.Limits{Wall: 200 * time.Millisecond}), &waitOnce{}}}
	}
	in := harness.Prompt("x")
	in.RunID = "r"
	res, err := build().Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	time.Sleep(300 * time.Millisecond)
	res, err = build().Run(ctx, harness.Resume("r", harness.Signal{Wait: res.Waits[0].ID, Value: "go"}))
	if err != nil || !res.OK() {
		t.Fatalf("%v stop=%s %s", err, res.Stop, res.Detail)
	}
}

type waitOnce struct{}

func (*waitOnce) Name() string { return "wait" }
func (*waitOnce) BeforeTool(_ context.Context, _ harness.Run, c harness.Call) harness.Decision {
	if _, ok := c.Signal("hold"); ok {
		return harness.Allow()
	}
	return harness.Await(harness.Wait{Kind: "hold"})
}

// A lead cannot see its children's spend in its own Run. A consumer's projection
// can, and the same hook on the lead then stops the tree. Without the projection the
// lead sails past the limit, which is the proof that the projection did the work.
func TestATreeBudgetUsesTheConsumersProjection(t *testing.T) {
	run := func(pooled bool) (*harness.Run, int) {
		l := &ledger{Journal: harness.NewMemJournal(), trees: map[string]model.Usage{}}
		paid := 0
		expensive := fake.NewFunc(func(model.Request) (model.Response, error) {
			paid++
			return model.Response{Text: "done", Usage: used(90, 0)}, nil
		})
		child := &harness.Agent{Name: "child", Provider: expensive}
		b := budget.New(budget.Limits{Tokens: 100})
		if pooled {
			b.WithPool(l.pool)
		}
		lead := &harness.Agent{Name: "lead", Journal: l, MaxTurns: 4, Hooks: []harness.Hook{b},
			Provider: fake.NewFunc(func(req model.Request) (model.Response, error) {
				r := fake.Call("delegate", map[string]string{"task": fmt.Sprint(len(req.Messages))})
				r.Usage = used(1, 0)
				return r, nil
			})}
		withSub, err := multi.WithSubagents(lead, multi.Subagent{Name: "delegate", Agent: child})
		if err != nil {
			t.Fatal(err)
		}
		in := harness.Prompt("x")
		in.RunID = "root"
		res, err := withSub.Run(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		return res, paid
	}
	res, paid := run(true)
	if res.Stop != harness.StopBudget || paid != 1 {
		t.Fatalf("stop %s after %d child calls (%s)", res.Stop, paid, res.Detail)
	}
	if res.Usage.Input >= 90 {
		t.Fatalf("the lead's own Run should hold only its own spend: %+v", res.Usage)
	}
	if res, paid := run(false); res.Stop == harness.StopBudget || paid != 4 {
		t.Fatalf("without the projection: stop %s after %d child calls", res.Stop, paid)
	}
}
