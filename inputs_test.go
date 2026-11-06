package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"sync"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/tool"
)

// metaSpy remembers the Meta of every Run its hook was handed.
type metaSpy struct{ seen *[]map[string]string }

func (metaSpy) Name() string { return "meta-spy" }
func (s metaSpy) BeforeTool(_ context.Context, r harness.Run, _ harness.Call) harness.Decision {
	*s.seen = append(*s.seen, maps.Clone(r.Meta))
	return harness.Allow()
}

func TestMetaIsRecordedAtStartAndAResumeCannotChangeIt(t *testing.T) {
	ctx := context.Background()
	j := newJSONJournal()
	w := newWorld()
	var seen []map[string]string
	a := approvalAgent(w, j, 0)
	a.Hooks = append([]harness.Hook{metaSpy{&seen}}, a.Hooks...)
	in := harness.Prompt("send it")
	in.RunID = "meta"
	in.Meta = map[string]string{"tenant": "acme", "principal": "42"}
	res, err := a.Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	resume := harness.Resume("meta", harness.Signal{Wait: res.Waits[0].ID, Value: "yes", By: "alice"})
	resume.Meta = map[string]string{"tenant": "intruder"}
	if res, err = a.Run(ctx, resume); err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	want := "map[principal:42 tenant:acme]"
	if got := fmt.Sprint(res.Meta); got != want {
		t.Fatalf("Run.Meta = %s, want %s", got, want)
	}
	if len(seen) != 2 || fmt.Sprint(seen[0]) != want || fmt.Sprint(seen[1]) != want {
		t.Fatalf("the hook saw %v", seen)
	}
	if got := fmt.Sprint(mustLoad(t, j, "meta")[0].Meta); got != want {
		t.Fatalf("run.started holds %s", got)
	}
}

func TestAChildInheritsMetaUnlessItsInputSetsItsOwn(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		own  map[string]string
		want string
	}{
		{"inherits", nil, "map[tenant:acme]"},
		{"own", map[string]string{"tenant": "child"}, "map[tenant:child]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := harness.NewMemJournal()
			w := newWorld()
			child := &harness.Agent{Name: "child", Provider: w.provider()}
			start := tool.Func(tool.Spec{Name: "delegate", Effect: tool.EffectIdempotent}, func(ctx context.Context, _ json.RawMessage) (string, error) {
				in := harness.Prompt("sub task")
				in.RunID, in.Meta = "child-run", tc.own
				res, err := child.Run(ctx, in)
				if err != nil {
					return "", err
				}
				return res.Output, nil
			})
			lead := &harness.Agent{Name: "lead", Journal: j, Provider: w.provider("delegate"), Tools: tool.NewRegistry(start)}
			in := harness.Prompt("go")
			in.RunID, in.Meta = "lead-run", map[string]string{"tenant": "acme"}
			if res, err := lead.Run(ctx, in); err != nil || !res.OK() {
				t.Fatalf("%v %+v", err, res)
			}
			r, err := harness.Fold(mustLoad(t, j, "child-run"))
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(r.Meta); got != tc.want {
				t.Fatalf("child Meta = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestMetaOverThirtyTwoKeysStartsNothing(t *testing.T) {
	j := harness.NewMemJournal()
	in := harness.Prompt("hi")
	in.RunID = "big"
	in.Meta = map[string]string{}
	for i := range 33 {
		in.Meta[fmt.Sprint("k", i)] = "v"
	}
	_, err := (&harness.Agent{Journal: j, Provider: newWorld().provider()}).Run(context.Background(), in)
	if err == nil || errors.Is(err, harness.ErrDrained) {
		t.Fatalf("want a refusal, got %v", err)
	}
	if evs := mustLoad(t, j, "big"); len(evs) != 0 {
		t.Fatalf("a refused start wrote %d events", len(evs))
	}
}

// requests records what the model was sent, as role:content lines per call.
type requests struct {
	mu    sync.Mutex
	calls [][]string
}

func (rq *requests) wrap(p model.Provider) model.Provider {
	return model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
		var lines []string
		for _, m := range req.Messages {
			if m.Content != "" {
				lines = append(lines, string(m.Role)+":"+m.Content)
			}
		}
		rq.mu.Lock()
		rq.calls = append(rq.calls, lines)
		rq.mu.Unlock()
		return p.Complete(ctx, req)
	})
}

func TestPinnedContextIsInEveryRequestAndAResumeCannotChangeIt(t *testing.T) {
	ctx := context.Background()
	j := newJSONJournal()
	w := newWorld()
	rq := &requests{}
	a := approvalAgent(w, j, 0)
	a.Instructions = "be brief"
	a.Provider = rq.wrap(a.Provider)
	in := harness.Prompt("send it")
	in.RunID = "pinned"
	in.Pinned = []model.Message{model.User("memory: prefers email")}
	res, err := a.Run(ctx, in)
	if err != nil || res.State != harness.StateSuspended {
		t.Fatalf("%v %+v", err, res)
	}
	resume := harness.Resume("pinned", harness.Signal{Wait: res.Waits[0].ID, Value: "yes"})
	resume.Pinned = []model.Message{model.User("memory: something else")}
	if res, err = a.Run(ctx, resume); err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	want := "[system:be brief user:memory: prefers email user:send it]"
	if len(rq.calls) != 2 {
		t.Fatalf("%d model calls", len(rq.calls))
	}
	for i, c := range rq.calls {
		if got := fmt.Sprint(c[:3]); got != want {
			t.Fatalf("request %d starts %s, want %s", i, got, want)
		}
	}
	if len(res.Pinned) != 1 || res.Pinned[0].Content != "memory: prefers email" {
		t.Fatalf("Run.Pinned = %+v", res.Pinned)
	}
	if res.Messages[0].Content != "send it" {
		t.Fatalf("Pinned leaked into Messages: %+v", res.Messages)
	}
}

func TestAChildDoesNotInheritPinnedContext(t *testing.T) {
	ctx := context.Background()
	w := newWorld()
	rq := &requests{}
	child := &harness.Agent{Name: "child", Provider: rq.wrap(w.provider())}
	lead := &harness.Agent{Name: "lead", Journal: harness.NewMemJournal(), Provider: w.provider("delegate"),
		Tools: tool.NewRegistry(subagentTool(child, "delegate"))}
	in := harness.Prompt("go")
	in.Pinned = []model.Message{model.User("lead only")}
	if res, err := lead.Run(ctx, in); err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if got := fmt.Sprint(rq.calls); got != "[[user:sub task]]" {
		t.Fatalf("the child was sent %s", got)
	}
}
