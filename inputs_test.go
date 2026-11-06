package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"testing"

	"github.com/ecociel/agent-harness"
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
