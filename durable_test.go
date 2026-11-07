package harness_test

import (
	"context"
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
	"github.com/ecociel/agent-harness/tool"
)

// jsonJournal round-trips every event through JSON, as any real store would, so
// the fold is proven against what survives serialization.
type jsonJournal struct {
	mu   sync.Mutex
	runs map[string][][]byte
}

func newJSONJournal() *jsonJournal { return &jsonJournal{runs: map[string][][]byte{}} }

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

var errCrash = errors.New("process died")

// crashJournal kills the process at the n-th Append across all runs: before the
// write, or after it with the caller never hearing back.
type crashJournal struct {
	harness.Journal
	at     int
	after  bool
	n      atomic.Int64
	killed atomic.Bool
}

func (c *crashJournal) Append(ctx context.Context, id string, evs []harness.Event) error {
	if c.killed.Load() {
		return errCrash
	}
	if int(c.n.Add(1)) == c.at {
		c.killed.Store(true)
		if c.after {
			_ = c.Journal.Append(ctx, id, evs)
		}
		return errCrash
	}
	return c.Journal.Append(ctx, id, evs)
}

// world is the outside: what tools really did and what the provider really billed.
type world struct {
	mu         sync.Mutex
	paid       int
	failedOnce bool
	effect     map[string][]string // tool name to the keys it ran with
}

func newWorld() *world { return &world{effect: map[string][]string{}} }

func (w *world) ran(name, key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.effect[name] = append(w.effect[name], key)
}

func (w *world) tool(name string, eff tool.Effect) tool.Tool {
	return tool.Func(tool.Spec{Name: name, Effect: eff}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		w.ran(name, tool.IdempotencyKey(ctx))
		return name + " ok", nil
	})
}

// provider asks for calls on the first turn and answers once it sees results. It
// decides from the request alone, like a model, so a repeated call is harmless to
// the script and is counted as paid.
func (w *world) provider(calls ...string) model.Provider {
	return model.ProviderFunc(func(_ context.Context, req model.Request) iter.Seq2[model.Event, error] {
		w.mu.Lock()
		w.paid++
		w.mu.Unlock()
		for _, m := range req.Messages {
			if m.Role == model.RoleTool {
				return model.Stream(model.Response{Text: "finished", Usage: model.Usage{Input: 10, Output: 1}})
			}
		}
		var tcs []model.ToolCall
		for i, name := range calls {
			tcs = append(tcs, model.ToolCall{ID: fmt.Sprintf("c%d", i), Name: name, Arguments: json.RawMessage(`{}`)})
		}
		return model.Stream(model.Response{ToolCalls: tcs, Usage: model.Usage{Input: 10, Output: 1}})
	})
}

func balanced(t *testing.T, msgs []model.Message) {
	t.Helper()
	open := map[string]bool{}
	for _, m := range msgs {
		if m.Role == model.RoleTool {
			if !open[m.ToolCallID] {
				t.Fatalf("result without call: %+v", m)
			}
			delete(open, m.ToolCallID)
			continue
		}
		if len(open) > 0 {
			t.Fatalf("unanswered calls %v before %+v", open, m)
		}
		for _, c := range m.ToolCalls {
			open[c.ID] = true
		}
	}
	if len(open) > 0 {
		t.Fatalf("unanswered calls %v at end", open)
	}
}

// drive resumes until the run ends, resolving the only wait a crash can open: an
// operator who checks the outside world and reports what happened.
func drive(t *testing.T, a *harness.Agent, id string) *harness.Run {
	t.Helper()
	for range 8 {
		res, err := a.Run(context.Background(), harness.Resume(id))
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		for res.State == harness.StateSuspended {
			w := res.Waits[0]
			if w.Kind != harness.WaitUnknownOutcome {
				t.Fatalf("unexpected wait %+v", w)
			}
			if res, err = a.Run(context.Background(), harness.Resume(id, harness.Signal{Wait: w.ID, Value: "verified by operator"})); err != nil {
				t.Fatalf("signal: %v", err)
			}
		}
		if res.State.Terminal() {
			return res
		}
	}
	t.Fatal("did not converge")
	return nil
}

// TestCrashAtEveryAppend is the crash table as a program: for every journal write
// in a run with a parallel pair of reads, one idempotent and one once-only tool,
// die before it and after it, restart with a fresh Agent value, and require
// convergence.
func TestCrashAtEveryAppend(t *testing.T) {
	const id = "crash-run"
	build := func(w *world, j harness.Journal) *harness.Agent {
		return &harness.Agent{Name: "a", Journal: j, Provider: w.provider("lookup", "stock", "ship", "pay"),
			Tools: tool.NewRegistry(w.tool("lookup", tool.EffectRead), w.tool("stock", tool.EffectRead),
				w.tool("ship", tool.EffectIdempotent), w.tool("pay", tool.EffectOnce))}
	}
	clean := newWorld()
	cj := &crashJournal{Journal: newJSONJournal()}
	res, err := build(clean, cj).Run(context.Background(), harness.Input{RunID: id, Messages: harness.Prompt("go").Messages})
	if err != nil || !res.OK() {
		t.Fatalf("clean run: %v %+v", err, res)
	}
	writes := int(cj.n.Load())
	if writes < 8 {
		t.Fatalf("only %d writes", writes)
	}

	in := harness.Prompt("go")
	in.RunID = id
	for at := 1; at <= writes; at++ {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprintf("write%d_after=%v", at, after), func(t *testing.T) {
				w := newWorld()
				inner := newJSONJournal()
				_, err := build(w, &crashJournal{Journal: inner, at: at, after: after}).Run(context.Background(), in)
				if !errors.Is(err, errCrash) {
					t.Fatalf("want crash, got %v", err)
				}
				// A restart that lost the first write starts the run again.
				if evs, _ := inner.Load(context.Background(), id); len(evs) == 0 {
					if _, err := build(w, inner).Run(context.Background(), in); err != nil {
						t.Fatal(err)
					}
				}
				// The restart dies too, on its first write.
				_, err = build(w, &crashJournal{Journal: inner, at: 1, after: after}).Run(context.Background(), harness.Resume(id))
				if err != nil && !errors.Is(err, errCrash) {
					t.Fatalf("restart: %v", err)
				}
				res := drive(t, build(w, inner), id)
				if res.Stop != harness.StopCompleted {
					t.Fatalf("stop = %s %q", res.Stop, res.Detail)
				}
				balanced(t, res.Messages)

				if n := len(w.effect["pay"]); n > 1 {
					t.Errorf("once-only tool ran %d times", n)
				}
				if keys := slices.Compact(slices.Clone(w.effect["ship"])); len(keys) != 1 {
					t.Errorf("idempotent tool used keys %v", keys)
				}
				// A lost call may have died before or after the provider saw it, so
				// the journal bounds what was billed and never counts a reply twice.
				if res.ModelCalls > w.paid || w.paid > res.ModelCalls+res.LostCalls {
					t.Errorf("billed %d outside [%d, %d]", w.paid, res.ModelCalls, res.ModelCalls+res.LostCalls)
				}
				if res.Usage.Input != 10*res.ModelCalls {
					t.Errorf("usage %d for %d calls", res.Usage.Input, res.ModelCalls)
				}
				evs, _ := inner.Load(context.Background(), id)
				again, err := harness.Fold(evs)
				if err != nil {
					t.Fatal(err)
				}
				if again.Output != res.Output || again.Usage != res.Usage {
					t.Error("replay disagrees with the live result")
				}
			})
		}
	}
}

// The second worker fences the first. The first cannot record, and an effect it
// was still running is treated as unknown, never repeated.
func TestTwoWorkersResumeOneRun(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	w := newWorld()
	release := make(chan struct{})
	entered := make(chan struct{})
	pay := tool.Func(tool.Spec{Name: "pay"}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		w.ran("pay", tool.IdempotencyKey(ctx))
		close(entered)
		<-release
		return "paid", nil
	})
	build := func() *harness.Agent {
		return &harness.Agent{Journal: j, Provider: w.provider("pay"), Tools: tool.NewRegistry(pay)}
	}
	in := harness.Prompt("go")
	in.RunID = "r"
	first := make(chan error)
	go func() {
		_, err := build().Run(ctx, in)
		first <- err
	}()
	<-entered

	res, err := build().Run(ctx, harness.Resume("r"))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != harness.StateSuspended || res.Waits[0].Kind != harness.WaitUnknownOutcome {
		t.Fatalf("want a wait on the unknown outcome, got %s %+v", res.State, res.Waits)
	}
	close(release)
	if err := <-first; !errors.Is(err, harness.ErrFenced) {
		t.Fatalf("first worker: %v", err)
	}
	if n := len(w.effect["pay"]); n != 1 {
		t.Fatalf("pay ran %d times", n)
	}
}

// approvals is the consumer's policy: a reference hook with no special access.
type approvals struct{ ttl time.Duration }

func (approvals) Name() string { return "approvals" }
func (a approvals) BeforeTool(_ context.Context, _ harness.Run, c harness.Call) harness.Decision {
	if c.Spec.Risk < tool.RiskWrite {
		return harness.Allow()
	}
	s, ok := c.Signal("approval")
	switch {
	case !ok || s.Expired:
		return harness.Await(harness.Wait{Kind: "approval", Detail: "run " + c.Name, TTL: a.ttl})
	case s.Value != "yes":
		return harness.Deny("declined by " + s.By)
	}
	return harness.Allow()
}

func approvalAgent(w *world, j harness.Journal, ttl time.Duration) *harness.Agent {
	send := tool.Func(tool.Spec{Name: "send", Risk: tool.RiskWrite}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		w.ran("send", tool.IdempotencyKey(ctx))
		return "sent", nil
	})
	return &harness.Agent{Journal: j, Provider: w.provider("send"), Tools: tool.NewRegistry(send),
		Hooks: []harness.Hook{approvals{ttl}}}
}

func TestApprovalAcrossProcesses(t *testing.T) {
	ctx := context.Background()
	j := newJSONJournal()
	w := newWorld()
	in := harness.Prompt("send it")
	in.RunID = "r"
	res, err := approvalAgent(w, j, 0).Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended || res.Stop != harness.StopSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	wait := res.Waits[0]

	// A timer firing early, or the same queue message twice, writes nothing.
	before, _ := j.Load(ctx, "r")
	if res, err = approvalAgent(w, j, 0).Run(ctx, harness.Resume("r")); err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	if after, _ := j.Load(ctx, "r"); len(after) != len(before) {
		t.Fatal("an unanswered resume wrote to the journal")
	}

	sig := harness.Signal{Wait: wait.ID, Value: "yes", By: "alice"}
	if res, err = approvalAgent(w, j, 0).Run(ctx, harness.Resume("r", sig)); err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if n := len(w.effect["send"]); n != 1 {
		t.Fatalf("send ran %d times", n)
	}
	// The same signal delivered again is the same finished run.
	if res, err = approvalAgent(w, j, 0).Run(ctx, harness.Resume("r", sig)); err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if n := len(w.effect["send"]); n != 1 {
		t.Fatalf("send ran %d times after a duplicate", n)
	}
}

func TestDeniedApprovalFeedsTheModel(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	w := newWorld()
	in := harness.Prompt("send it")
	in.RunID = "r"
	res, _ := approvalAgent(w, j, 0).Run(ctx, in)
	res, err := approvalAgent(w, j, 0).Run(ctx, harness.Resume("r",
		harness.Signal{Wait: res.Waits[0].ID, Value: "no", By: "bob"}))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if len(w.effect["send"]) != 0 {
		t.Fatal("denied tool ran")
	}
	balanced(t, res.Messages)
	if !strings.Contains(res.Messages[2].Content, "declined by bob") {
		t.Fatalf("model not told: %+v", res.Messages[2])
	}
}

// A wait is bound to one ask. An answer to an expired ask is refused, and the
// new ask has a new identity, so the old answer cannot satisfy it.
func TestStaleApproval(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	w := newWorld()
	in := harness.Prompt("send it")
	in.RunID = "r"
	res, _ := approvalAgent(w, j, time.Millisecond).Run(ctx, in)
	first := res.Waits[0]
	time.Sleep(2 * time.Millisecond)

	late := harness.Signal{Wait: first.ID, Value: "yes"}
	if _, err := approvalAgent(w, j, time.Hour).Run(ctx, harness.Resume("r", late)); !errors.Is(err, harness.ErrStaleSignal) {
		t.Fatalf("late signal: %v", err)
	}
	// The scheduler's timer expires the wait, and the policy asks again.
	res, err := approvalAgent(w, j, time.Hour).Run(ctx, harness.Resume("r"))
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Waits[0].ID == first.ID {
		t.Fatal("the re-ask reused the wait's identity")
	}
	if _, err := approvalAgent(w, j, time.Hour).Run(ctx, harness.Resume("r", late)); !errors.Is(err, harness.ErrStaleSignal) {
		t.Fatalf("old answer to the new ask: %v", err)
	}
	if len(w.effect["send"]) != 0 {
		t.Fatal("stale approval ran the tool")
	}
}

func TestSignalForNoWaitIsRejected(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	w := newWorld()
	in := harness.Prompt("send it")
	in.RunID = "r"
	_, _ = approvalAgent(w, j, 0).Run(ctx, in)
	_, err := approvalAgent(w, j, 0).Run(ctx, harness.Resume("r", harness.Signal{Wait: "forged", Value: "yes"}))
	if !errors.Is(err, harness.ErrStaleSignal) {
		t.Fatalf("got %v", err)
	}
}

// The tool changed while the run slept. The approved call is not executed against
// the new tool. The model is told, and replans.
func TestToolChangedBetweenSuspendAndResume(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	w := newWorld()
	in := harness.Prompt("send it")
	in.RunID = "r"
	res, _ := approvalAgent(w, j, 0).Run(ctx, in)

	changed := tool.Func(tool.Spec{Name: "send", Risk: tool.RiskWrite, Description: "now emails everyone"},
		func(context.Context, json.RawMessage) (string, error) {
			t.Error("ran the changed tool")
			return "", nil
		})
	a := approvalAgent(w, j, 0)
	a.Tools = tool.NewRegistry(changed)
	res, err := a.Run(ctx, harness.Resume("r", harness.Signal{Wait: res.Waits[0].ID, Value: "yes"}))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	balanced(t, res.Messages)
	if !strings.Contains(res.Messages[2].Content, "changed") {
		t.Fatalf("model not told: %+v", res.Messages[2])
	}
}

// A run keeps the instructions it started with, whatever is deployed now.
func TestInstructionsArePinned(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	var seen []string
	p := model.ProviderFunc(func(_ context.Context, req model.Request) iter.Seq2[model.Event, error] {
		seen = append(seen, req.Messages[0].Content)
		if len(seen) == 1 {
			return model.Stream(model.Response{ToolCalls: []model.ToolCall{{ID: "1", Name: "ask", Arguments: json.RawMessage(`{}`)}}})
		}
		return model.Stream(model.Response{Text: "ok"})
	})
	ask := tool.Func(tool.Spec{Name: "ask"}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		if s := harness.Signals(ctx); len(s) > 0 {
			return s[0].Value, nil
		}
		return "", harness.Suspend(harness.Wait{Kind: "user", Detail: "Which account?"})
	})
	a := &harness.Agent{Instructions: "v1", Journal: j, Provider: p, Tools: tool.NewRegistry(ask)}
	in := harness.Prompt("hi")
	in.RunID = "r"
	res, err := a.Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	b := *a
	b.Instructions = "v2"
	res, err = b.Run(ctx, harness.Resume("r", harness.Signal{Wait: res.Waits[0].ID, Value: "savings"}))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if !slices.Equal(seen, []string{"v1", "v1"}) {
		t.Fatalf("instructions seen: %v", seen)
	}
	r, _ := harness.Fold(mustLoad(t, j, "r"))
	if !r.Drift.Instructions {
		t.Fatal("drift not recorded")
	}
	if res.Messages[2].Content != "savings" {
		t.Fatalf("tool result: %+v", res.Messages[2])
	}
}

func mustLoad(t *testing.T, j harness.Journal, id string) []harness.Event {
	t.Helper()
	evs, err := j.Load(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func subagentTool(child harness.Runner, name string) tool.Tool {
	return tool.Func(tool.Spec{Name: name, Effect: tool.EffectIdempotent}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		in := harness.Prompt("sub task")
		in.RunID, in.Signals = tool.IdempotencyKey(ctx), harness.Signals(ctx)
		res, err := child.Run(ctx, in)
		if err != nil {
			return "", err
		}
		if res.State == harness.StateSuspended {
			return "", harness.Suspend(res.Waits...)
		}
		return res.Output, nil
	})
}

// A crash anywhere inside a child resumes the same child from its own journal.
func TestCrashInsideSubAgent(t *testing.T) {
	ctx := context.Background()
	build := func(w *world, j harness.Journal) *harness.Agent {
		child := &harness.Agent{Name: "child", Provider: w.provider("pay"),
			Tools: tool.NewRegistry(w.tool("pay", tool.EffectOnce))}
		return &harness.Agent{Name: "lead", Journal: j, Provider: w.provider("delegate"),
			Tools: tool.NewRegistry(subagentTool(child, "delegate"))}
	}
	for at := 1; at <= 40; at++ {
		for _, after := range []bool{false, true} {
			w := newWorld()
			inner := newJSONJournal()
			in := harness.Prompt("go")
			in.RunID = "lead-run"
			_, err := build(w, &crashJournal{Journal: inner, at: at, after: after}).Run(ctx, in)
			if err == nil {
				continue // the run finished before write at
			}
			if evs := mustLoad(t, inner, "lead-run"); len(evs) == 0 {
				if _, err := build(w, inner).Run(ctx, in); err != nil {
					t.Fatal(err)
				}
			}
			var res *harness.Run
			for i := 0; ; i++ {
				if i > 8 {
					t.Fatalf("write %d: did not converge", at)
				}
				var sig []harness.Signal
				if res != nil {
					for _, w := range res.Waits {
						sig = append(sig, harness.Signal{Wait: w.ID, Value: "verified"})
					}
				}
				if res, err = build(w, inner).Run(ctx, harness.Resume("lead-run", sig...)); err != nil {
					t.Fatalf("write %d after=%v: %v", at, after, err)
				}
				if res.State.Terminal() {
					break
				}
			}
			if res.Stop != harness.StopCompleted {
				t.Fatalf("write %d: %s %s", at, res.Stop, res.Detail)
			}
			if n := len(w.effect["pay"]); n > 1 {
				t.Fatalf("write %d after=%v: child effect ran %d times", at, after, n)
			}
		}
	}
}

// An approval inside a child surfaces at the root and is answered at the root.
func TestSuspensionInsideSubAgent(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	w := newWorld()
	child := approvalAgent(w, nil, 0)
	child.Journal = nil
	lead := &harness.Agent{Journal: j, Provider: w.provider("delegate"), Tools: tool.NewRegistry(subagentTool(child, "delegate"))}
	in := harness.Prompt("go")
	in.RunID = "root"
	res, err := lead.Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	wait := res.Waits[0]
	if wait.Run == "root" {
		t.Fatalf("wait should name the child: %+v", wait)
	}
	res, err = lead.Run(ctx, harness.Resume("root", harness.Signal{Wait: wait.ID, Value: "yes", By: "alice"}))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if n := len(w.effect["send"]); n != 1 {
		t.Fatalf("send ran %d times", n)
	}
	var kids []harness.Event
	for _, e := range mustLoad(t, j, wait.Run) {
		kids = append(kids, e)
	}
	if kids[0].ParentRun != "root" || kids[0].RootRun != "root" {
		t.Fatalf("child identity: %+v", kids[0])
	}
}

func TestDrainParksAndAnyProcessResumes(t *testing.T) {
	j := harness.NewMemJournal()
	w := newWorld()
	ctx, drain := harness.WithDrain(context.Background())
	build := func(tools *tool.Registry) *harness.Agent {
		return &harness.Agent{Journal: j, Provider: w.provider("slow"), Tools: tools}
	}
	slow := tool.Func(tool.Spec{Name: "slow"}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		drain()
		return "done", nil
	})
	in := harness.Prompt("go")
	in.RunID = "r"
	if _, err := build(tool.NewRegistry(slow)).Run(ctx, in); !errors.Is(err, harness.ErrDrained) {
		t.Fatalf("got %v", err)
	}
	r, err := harness.Fold(mustLoad(t, j, "r"))
	if err != nil || r.State.Terminal() || r.ToolCalls != 1 {
		t.Fatalf("the in-flight step was not recorded: %v %+v", err, r)
	}
	res, err := build(tool.NewRegistry(slow)).Run(context.Background(), harness.Resume("r"))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
}

// A deploy cancels every context in the process. That must not end a single
// durable run, and a tool cancelled mid-effect has an unknown outcome.
func TestCancelledContextStopsTheWorkerNotTheRun(t *testing.T) {
	j := harness.NewMemJournal()
	w := newWorld()
	ctx, cancel := context.WithCancel(context.Background())
	pay := tool.Func(tool.Spec{Name: "pay"}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		w.ran("pay", tool.IdempotencyKey(ctx))
		cancel()
		<-ctx.Done()
		return "", ctx.Err()
	})
	a := &harness.Agent{Journal: j, Provider: w.provider("pay"), Tools: tool.NewRegistry(pay)}
	in := harness.Prompt("go")
	in.RunID = "r"
	if _, err := a.Run(ctx, in); !errors.Is(err, harness.ErrDrained) {
		t.Fatalf("got %v", err)
	}
	res, err := a.Run(context.Background(), harness.Resume("r"))
	if err != nil || res.State != harness.StateSuspended || res.Waits[0].Kind != harness.WaitUnknownOutcome {
		t.Fatalf("%v %+v", err, res)
	}
	res, err = a.Run(context.Background(), harness.Resume("r", harness.Signal{Wait: res.Waits[0].ID, Value: "the charge went through"}))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if n := len(w.effect["pay"]); n != 1 {
		t.Fatalf("pay ran %d times", n)
	}
}

// The same holds through a sub-agent: its stop is not the lead's tool result.
func TestDrainThroughASubAgentLeavesTheLeadResumable(t *testing.T) {
	j := harness.NewMemJournal()
	w := newWorld()
	ctx, drain := harness.WithDrain(context.Background())
	calls := 0
	work := tool.Func(tool.Spec{Name: "work", Effect: tool.EffectIdempotent}, func(context.Context, json.RawMessage) (string, error) {
		calls++
		drain()
		return "worked", nil
	})
	child := &harness.Agent{Provider: w.provider("work"), Tools: tool.NewRegistry(work)}
	lead := &harness.Agent{Journal: j, Provider: w.provider("delegate"), Tools: tool.NewRegistry(subagentTool(child, "delegate"))}
	in := harness.Prompt("go")
	in.RunID = "root"
	if _, err := lead.Run(ctx, in); !errors.Is(err, harness.ErrDrained) {
		t.Fatalf("got %v", err)
	}
	res, err := lead.Run(context.Background(), harness.Resume("root"))
	if err != nil || !res.OK() || res.Output != "finished" {
		t.Fatalf("%v %+v", err, res)
	}
	if calls != 1 {
		t.Fatalf("work ran %d times", calls)
	}
	if got := res.Messages[2].Content; got != "finished" {
		t.Fatalf("the lead was told %q instead of the child's answer", got)
	}
}

func TestCancelFromAnotherProcess(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	w := newWorld()
	in := harness.Prompt("send it")
	in.RunID = "r"
	_, _ = approvalAgent(w, j, 0).Run(ctx, in)
	res, err := approvalAgent(w, j, 0).Run(ctx, harness.Input{RunID: "r", Cancel: "customer withdrew"})
	if err != nil || res.State != harness.StateStopped || res.Stop != harness.StopCancelled {
		t.Fatalf("%v %+v", err, res)
	}
	balanced(t, res.Messages)
}

// Durable backoff: a long wait parks with a timer the consumer's scheduler honours.
type retryOnce struct{ after time.Duration }

func (retryOnce) Name() string { return "retry" }
func (r retryOnce) Recover(_ context.Context, _ harness.Run, f harness.Failure) harness.Recovery {
	return harness.Recovery{Retry: f.Class == model.ClassRateLimit, After: r.after}
}

func TestRetryIsRecordedAndBackoffSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	calls := 0
	p := model.ProviderFunc(func(context.Context, model.Request) iter.Seq2[model.Event, error] {
		calls++
		if calls == 1 {
			return model.Fail(&model.Error{Class: model.ClassRateLimit, Usage: model.Usage{Input: 3}, Err: errors.New("429")})
		}
		return model.Stream(model.Response{Text: "ok", Usage: model.Usage{Input: 10}})
	})
	a := &harness.Agent{Journal: j, Provider: p, Hooks: []harness.Hook{retryOnce{time.Hour}}}
	in := harness.Prompt("hi")
	in.RunID = "r"
	res, err := a.Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended || res.Waits[0].Kind != harness.WaitTimer {
		t.Fatalf("%v %+v", err, res)
	}
	if !res.Waits[0].Expires.After(time.Now()) {
		t.Fatal("timer has no deadline")
	}
	// The scheduler fires early: nothing happens.
	if res, _ = a.Run(ctx, harness.Resume("r")); res.State != harness.StateSuspended || calls != 1 {
		t.Fatalf("early resume: %+v calls=%d", res, calls)
	}
	// The hour has passed for a journal that says so.
	evs := mustLoad(t, j, "r")
	for i := range evs {
		for k := range evs[i].Waits {
			evs[i].Waits[k].Expires = time.Now().Add(-time.Second)
		}
	}
	past := harness.NewMemJournal()
	if err := past.Append(ctx, "r", evs); err != nil {
		t.Fatal(err)
	}
	b := *a
	b.Journal = past
	res, err = b.Run(ctx, harness.Resume("r"))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Usage.Input != 13 {
		t.Fatalf("failed attempt's usage lost: %+v", res.Usage)
	}
}

func TestAwaitWithoutJournalFailsPlainly(t *testing.T) {
	w := newWorld()
	a := approvalAgent(w, nil, 0)
	a.Journal = nil
	res, err := a.Run(context.Background(), harness.Prompt("send it"))
	if !errors.Is(err, harness.ErrNotDurable) || res.State != harness.StateFailed {
		t.Fatalf("%v %+v", err, res)
	}
	balanced(t, res.Messages)
}

func TestSessionRepeatedSendAfterCrashDoesNotPayTwice(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	w := newWorld()
	a := &harness.Agent{Journal: j, Provider: w.provider()}
	s := harness.NewSession(a)
	if _, err := s.Send(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	paid := w.paid
	restored := &harness.Session{ID: s.ID, Runner: a, Turn: 0} // the save was lost
	if _, err := restored.Send(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	if w.paid != paid {
		t.Fatalf("paid again: %d -> %d", paid, w.paid)
	}
	if restored.Turn != 1 {
		t.Fatalf("session did not catch up: %+v", restored)
	}
	res, err := restored.Send(ctx, "two")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 4 {
		t.Fatalf("history: %+v", res.Messages)
	}
}

type afterRunCounter struct{ n *int }

func (afterRunCounter) Name() string                            { return "after" }
func (c afterRunCounter) AfterRun(context.Context, harness.Run) { *c.n++ }

// AfterRun runs before run.ended is written, so memory saves survive a crash at
// the very last step, and a finished run never repeats them.
func TestAfterRunIsAtLeastOnce(t *testing.T) {
	ctx := context.Background()
	n := 0
	w := newWorld()
	inner := harness.NewMemJournal()
	build := func(j harness.Journal) *harness.Agent {
		return &harness.Agent{Journal: j, Provider: w.provider(), Hooks: []harness.Hook{afterRunCounter{&n}}}
	}
	in := harness.Prompt("hi")
	in.RunID = "r"
	clean := &crashJournal{Journal: harness.NewMemJournal()}
	if _, err := build(clean).Run(ctx, in); err != nil {
		t.Fatal(err)
	}
	last := int(clean.n.Load())
	n = 0
	if _, err := build(&crashJournal{Journal: inner, at: last}).Run(ctx, in); !errors.Is(err, errCrash) {
		t.Fatalf("got %v", err)
	}
	res, err := build(inner).Run(ctx, harness.Resume("r"))
	if err != nil || !res.OK() || n != 2 {
		t.Fatalf("%v %+v after-run=%d", err, res, n)
	}
	if _, err := build(inner).Run(ctx, harness.Resume("r")); err != nil || n != 2 {
		t.Fatalf("%v after-run=%d", err, n)
	}
}

// A child's wait that expired reaches the child as its own expiry, and the marker
// the parent forwards is not a signal the child can refuse.
func TestNestedWaitExpires(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	w := newWorld()
	child := approvalAgent(w, nil, time.Millisecond)
	child.Journal = nil
	lead := &harness.Agent{Journal: j, Provider: w.provider("delegate"), Tools: tool.NewRegistry(subagentTool(child, "delegate"))}
	in := harness.Prompt("go")
	in.RunID = "root"
	res, err := lead.Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	first := res.Waits[0].ID
	time.Sleep(2 * time.Millisecond)
	child.Hooks = []harness.Hook{approvals{time.Hour}}
	res, err = lead.Run(ctx, harness.Resume("root"))
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Waits[0].ID == first || res.Waits[0].Kind != "approval" {
		t.Fatalf("want a fresh ask, got %+v", res.Waits[0])
	}
}

// The lead's own approval of the delegate call is the hook's business. It must not
// reach the tool, or the child would be handed a wait it has never heard of.
func TestToolOnlySeesSignalsForItsOwnWaits(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	var got [][]harness.Signal
	ask := tool.Func(tool.Spec{Name: "ask", Risk: tool.RiskWrite}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		got = append(got, harness.Signals(ctx))
		if s := harness.Signals(ctx); len(s) > 0 {
			return s[0].Value, nil
		}
		return "", harness.Suspend(harness.Wait{Kind: "user", Detail: "Which account?"})
	})
	w := newWorld()
	a := &harness.Agent{Journal: j, Provider: w.provider("ask"), Tools: tool.NewRegistry(ask), Hooks: []harness.Hook{approvals{}}}
	in := harness.Prompt("hi")
	in.RunID = "r"
	res, err := a.Run(ctx, in)
	if err != nil || res.Waits[0].Kind != "approval" {
		t.Fatalf("%v %+v", err, res)
	}
	res, err = a.Run(ctx, harness.Resume("r", harness.Signal{Wait: res.Waits[0].ID, Value: "yes"}))
	if err != nil || res.Waits[0].Kind != "user" {
		t.Fatalf("%v %+v", err, res)
	}
	if len(got) != 1 || len(got[0]) != 0 {
		t.Fatalf("tool saw the approval: %+v", got)
	}
	res, err = a.Run(ctx, harness.Resume("r", harness.Signal{Wait: res.Waits[0].ID, Value: "savings"}))
	if err != nil || !res.OK() || res.Messages[2].Content != "savings" {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestResumeOfUnknownRunStartsNothing(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	a := &harness.Agent{Journal: j, Provider: newWorld().provider()}
	for _, in := range []harness.Input{harness.Resume("ghost"), {RunID: "ghost", Cancel: "x"}} {
		if _, err := a.Run(ctx, in); !errors.Is(err, harness.ErrNoSuchRun) {
			t.Fatalf("got %v", err)
		}
	}
	if evs := mustLoad(t, j, "ghost"); len(evs) != 0 {
		t.Fatalf("wrote %d events", len(evs))
	}
}

func TestCorruptJournalIsAnErrorNotAPanic(t *testing.T) {
	for _, evs := range [][]harness.Event{
		{{Seq: 1, Kind: harness.KindRunStarted}},
		{{Seq: 1, Kind: harness.KindRunStarted, Pin: &harness.Pin{Schema: 1}}, {Seq: 2, Kind: harness.KindHistoryEdited}},
		{{Seq: 2, Kind: harness.KindRunStarted}},
		{{Seq: 1, Kind: harness.KindRunStarted, Pin: &harness.Pin{Schema: 99}}},
	} {
		if _, err := harness.Fold(evs); err == nil {
			t.Fatalf("folded %+v", evs)
		}
	}
}

func TestResumedRunKnowsHowLongItSlept(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	w := newWorld()
	in := harness.Prompt("send it")
	in.RunID = "r"
	res, _ := approvalAgent(w, j, 0).Run(ctx, in)
	time.Sleep(time.Millisecond)
	var saw harness.Run
	a := approvalAgent(w, j, 0)
	a.Hooks = append(a.Hooks, ctxSpy{&saw})
	if _, err := a.Run(ctx, harness.Resume("r", harness.Signal{Wait: res.Waits[0].ID, Value: "yes"})); err != nil {
		t.Fatal(err)
	}
	if saw.Waited <= 0 || saw.Claims != 1 {
		t.Fatalf("waited %s claims %d", saw.Waited, saw.Claims)
	}
}

type ctxSpy struct{ into *harness.Run }

func (ctxSpy) Name() string { return "spy" }
func (c ctxSpy) Context(_ context.Context, r harness.Run) (harness.Edit, error) {
	*c.into = r
	return harness.Edit{}, nil
}

// revocable allows "a" until revoked, and makes "b" wait for a human.
type revocable struct{ revoked *bool }

func (revocable) Name() string { return "revocable" }
func (h revocable) BeforeTool(_ context.Context, _ harness.Run, c harness.Call) harness.Decision {
	switch {
	case c.Name == "a" && *h.revoked:
		return harness.Deny("permission revoked")
	case c.Name == "b":
		if _, ok := c.Signal("human"); !ok {
			return harness.Await(harness.Wait{Kind: "human"})
		}
	}
	return harness.Allow()
}

// A decision made before a suspension is not trusted after it. Call a was allowed
// when the run parked on call b, and that permission no longer holds on resume.
func TestDecisionsDoNotSurviveASuspension(t *testing.T) {
	ctx := context.Background()
	j := harness.NewMemJournal()
	w := newWorld()
	revoked := false
	a := &harness.Agent{Journal: j, Provider: w.provider("a", "b"),
		Tools: tool.NewRegistry(w.tool("a", tool.EffectOnce), w.tool("b", tool.EffectOnce)),
		Hooks: []harness.Hook{revocable{&revoked}}}
	in := harness.Prompt("go")
	in.RunID = "r"
	res, err := a.Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended || len(w.effect["a"]) != 0 {
		t.Fatalf("%v %+v effects %v", err, res, w.effect)
	}
	revoked = true
	res, err = a.Run(ctx, harness.Resume("r", harness.Signal{Wait: res.Waits[0].ID, Value: "go"}))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if len(w.effect["a"]) != 0 {
		t.Fatal("ran a call on a permission that had been revoked")
	}
	if len(w.effect["b"]) != 1 {
		t.Fatalf("b ran %d times", len(w.effect["b"]))
	}
}

// A pattern keeps no state of its own. Give each step a derived run ID and the
// pattern is durable by re-running it: finished steps return from their journals,
// and the interrupted step resumes from its own.
func TestPatternRerunsOverMemoizedSteps(t *testing.T) {
	ctx := context.Background()
	inner := harness.NewMemJournal()
	counts := map[string]int{}
	step := func(name string) *harness.Agent {
		return &harness.Agent{Name: name, Provider: model.ProviderFunc(func(context.Context, model.Request) iter.Seq2[model.Event, error] {
			counts[name]++
			return model.Stream(model.Response{Text: name + " done"})
		})}
	}
	sequential := func(steps ...*harness.Agent) harness.Runner {
		return harness.RunnerFunc(func(ctx context.Context, in harness.Input) (*harness.Run, error) {
			var last *harness.Run
			for i, s := range steps {
				sub := harness.Prompt(in.Text())
				sub.RunID = fmt.Sprintf("%s/%d", in.RunID, i)
				res, err := s.Run(ctx, sub)
				if err != nil {
					return nil, err
				}
				last = res
			}
			return last, nil
		})
	}
	var a, b *harness.Agent
	build := func(j harness.Journal) harness.Runner {
		a, b = step("a"), step("b")
		a.Journal, b.Journal = j, j
		return sequential(a, b)
	}
	clean := &crashJournal{Journal: harness.NewMemJournal()}
	in := harness.Prompt("go")
	in.RunID = "p"
	if _, err := build(clean).Run(ctx, in); err != nil {
		t.Fatal(err)
	}
	total := int(clean.n.Load())
	for k := range counts {
		delete(counts, k)
	}
	// Die in the middle of step b, after step a has finished.
	if _, err := build(&crashJournal{Journal: inner, at: total - 1}).Run(ctx, in); !errors.Is(err, errCrash) {
		t.Fatalf("got %v", err)
	}
	res, err := build(inner).Run(ctx, in)
	if err != nil || res.Output != "b done" {
		t.Fatalf("%v %+v", err, res)
	}
	if counts["a"] != 1 {
		t.Fatalf("the finished step ran %d times", counts["a"])
	}
	if counts["b"] != 1 {
		t.Fatalf("the interrupted step's reply was journaled, yet the model ran %d times", counts["b"])
	}
}

// richHooks is every recorded decision a run can make, in one consumer hook.
type richHooks struct{}

func (richHooks) Name() string { return "rich" }
func (richHooks) Context(_ context.Context, r harness.Run) (harness.Edit, error) {
	if len(r.Messages) < 3 {
		return harness.Edit{}, nil
	}
	if slices.ContainsFunc(r.Messages, func(m model.Message) bool { return m.Source == "compact" }) {
		return harness.Edit{}, nil
	}
	return harness.Edit{To: len(r.Messages), Source: "compact", With: []model.Message{model.User("summary")}}, nil
}
func (richHooks) Recover(context.Context, harness.Run, harness.Failure) harness.Recovery {
	return harness.Recovery{Retry: true}
}
func (richHooks) BeforeTool(_ context.Context, _ harness.Run, c harness.Call) harness.Decision {
	if _, ok := c.Signal("approval"); !ok {
		return harness.Await(harness.Wait{Kind: "approval"})
	}
	return harness.Allow()
}
func (richHooks) CheckOutput(_ context.Context, r harness.Run, _ string) harness.Verdict {
	if r.Repairs == 0 {
		return harness.Revise("again")
	}
	return harness.Accept()
}

// TestCrashMatrixEveryKindOfRecord kills a run at every write of a path that
// compacts, retries, waits for a human and repairs its answer, and then kills the
// restart too, so the resume path is under the same test as the first path.
func TestCrashMatrixEveryKindOfRecord(t *testing.T) {
	ctx := context.Background()
	const id = "rich"
	in := harness.Prompt("go")
	in.RunID = id
	build := func(w *world, j harness.Journal) *harness.Agent {
		failed := false
		p := model.ProviderFunc(func(_ context.Context, req model.Request) iter.Seq2[model.Event, error] {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.paid++
			if !w.failedOnce && !failed {
				w.failedOnce, failed = true, true
				return model.Fail(&model.Error{Class: model.ClassTransient, Err: errors.New("503")})
			}
			if len(req.Messages) == 1 && req.Messages[0].Content == "go" {
				return model.Stream(model.Response{Usage: model.Usage{Input: 10}, ToolCalls: []model.ToolCall{
					{ID: "c0", Name: "ship", Arguments: json.RawMessage(`{}`)}}})
			}
			return model.Stream(model.Response{Text: "finished", Usage: model.Usage{Input: 10}})
		})
		return &harness.Agent{Journal: j, Provider: p, Tools: tool.NewRegistry(w.tool("ship", tool.EffectIdempotent)),
			Hooks: []harness.Hook{richHooks{}}}
	}
	finish := func(t *testing.T, w *world, j harness.Journal) *harness.Run {
		t.Helper()
		var res *harness.Run
		for range 20 {
			var sig []harness.Signal
			if res != nil {
				for _, wt := range res.Waits {
					sig = append(sig, harness.Signal{Wait: wt.ID, Value: "yes"})
				}
			}
			var err error
			if res, err = build(w, j).Run(ctx, harness.Resume(id, sig...)); err != nil {
				t.Fatalf("resume: %v", err)
			}
			if res.State.Terminal() {
				return res
			}
		}
		t.Fatal("did not converge")
		return nil
	}

	cw := newWorld()
	clean := &crashJournal{Journal: harness.NewMemJournal()}
	res, err := build(cw, clean).Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	if finish(t, cw, clean).Stop != harness.StopCompleted {
		t.Fatal("clean path did not complete")
	}
	kinds := map[harness.Kind]bool{}
	for _, e := range mustLoad(t, clean.Journal, id) {
		kinds[e.Kind] = true
	}
	for _, k := range []harness.Kind{harness.KindHistoryEdited, harness.KindModelFailed, harness.KindWaitResolved,
		harness.KindRunClaimed, harness.KindToolGated, harness.KindOutputChecked} {
		if !kinds[k] {
			t.Fatalf("the path never writes %s", k)
		}
	}
	writes := int(clean.n.Load())
	for at := 1; at <= writes; at++ {
		for _, after := range []bool{false, true} {
			for _, second := range []int{0, 1, 2} {
				t.Run(fmt.Sprintf("write%d_after=%v_restartdies=%d", at, after, second), func(t *testing.T) {
					w := newWorld()
					inner := newJSONJournal()
					// One counter across the first run and its resumes, so at names
					// one write of the whole path, including those after the wait.
					dying := &crashJournal{Journal: inner, at: at, after: after}
					res, err := build(w, dying).Run(ctx, in)
					for err == nil && !res.State.Terminal() {
						var sig []harness.Signal
						for _, wt := range res.Waits {
							sig = append(sig, harness.Signal{Wait: wt.ID, Value: "yes"})
						}
						res, err = build(w, dying).Run(ctx, harness.Resume(id, sig...))
					}
					if !errors.Is(err, errCrash) {
						t.Fatalf("want a crash at write %d, got %v", at, err)
					}
					if evs := mustLoad(t, inner, id); len(evs) == 0 {
						if _, err := build(w, inner).Run(ctx, in); err != nil {
							t.Fatal(err)
						}
					}
					if second > 0 {
						_, err := build(w, &crashJournal{Journal: inner, at: second, after: after}).Run(ctx, harness.Resume(id))
						if err != nil && !errors.Is(err, errCrash) {
							t.Fatalf("restart: %v", err)
						}
					}
					res = finish(t, w, inner)
					if res.Stop != harness.StopCompleted {
						t.Fatalf("stop = %s %q", res.Stop, res.Detail)
					}
					balanced(t, res.Messages)
					if keys := slices.Compact(slices.Clone(w.effect["ship"])); len(keys) != 1 {
						t.Errorf("idempotent tool used keys %v", keys)
					}
					edits := 0
					for _, e := range mustLoad(t, inner, id) {
						if e.Kind == harness.KindHistoryEdited {
							edits++
						}
					}
					if edits != 1 {
						t.Errorf("compacted %d times", edits)
					}
				})
			}
		}
	}
}

// A limit is decided by the pure step, so a crash around its record repeats nothing.
func TestCrashAroundRunStopping(t *testing.T) {
	ctx := context.Background()
	build := func(w *world, j harness.Journal) *harness.Agent {
		return &harness.Agent{Journal: j, MaxTurns: 1, Provider: w.provider("look"),
			Tools: tool.NewRegistry(w.tool("look", tool.EffectRead))}
	}
	in := harness.Prompt("go")
	in.RunID = "r"
	clean := &crashJournal{Journal: harness.NewMemJournal()}
	if _, err := build(newWorld(), clean).Run(ctx, in); err != nil {
		t.Fatal(err)
	}
	evs := mustLoad(t, clean.Journal, "r")
	stopping := slices.IndexFunc(evs, func(e harness.Event) bool { return e.Kind == harness.KindRunStopping })
	if stopping < 0 {
		t.Fatal("the path never writes run.stopping")
	}
	// Each append here is one event, so the write index is the event index.
	writes := int(clean.n.Load()) - (len(evs) - stopping) + 1
	for _, after := range []bool{false, true} {
		w := newWorld()
		inner := harness.NewMemJournal()
		if _, err := build(w, &crashJournal{Journal: inner, at: writes, after: after}).Run(ctx, in); !errors.Is(err, errCrash) {
			t.Fatalf("after=%v: %v", after, err)
		}
		res, err := build(w, inner).Run(ctx, harness.Resume("r"))
		if err != nil || res.Stop != harness.StopMaxTurns || res.State != harness.StateStopped {
			t.Fatalf("after=%v: %v %+v", after, err, res)
		}
		balanced(t, res.Messages)
		if n := w.paid; n != 1 {
			t.Fatalf("after=%v: paid %d times", after, n)
		}
	}
}
