package trajectory_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/trajectory"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

var echo = tool.Func(tool.Spec{Name: "echo", Effect: tool.EffectIdempotent}, func(_ context.Context, a json.RawMessage) (string, error) { return string(a), nil })

func TestRecords(t *testing.T) {
	var out bytes.Buffer
	rec := trajectory.New()
	rec.Write = &out
	a := &harness.Agent{Provider: fake.Script(fake.Call("echo", map[string]int{"n": 1}), fake.Text("ok")),
		Tools: tool.NewRegistry(echo), Hooks: []harness.Hook{rec}}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := rec.Get(res.ID)
	if !ok || len(tr.Steps) != 1 || tr.Steps[0].Result != `{"n":1}` || string(tr.Steps[0].Args) != `{"n":1}` ||
		tr.Steps[0].Outcome != "ok" || tr.Output != "ok" || tr.Stop != harness.StopCompleted || tr.Input[0].Content != "x" {
		t.Fatalf("%+v", tr)
	}
	if !strings.Contains(out.String(), `"run_id"`) || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("%q", out.String())
	}
}

type denyAll struct{}

func (denyAll) Name() string { return "deny" }
func (denyAll) BeforeTool(context.Context, harness.Run, harness.Call) harness.Decision {
	return harness.Deny("no")
}

func TestADenialIsAStep(t *testing.T) {
	rec := trajectory.New()
	a := &harness.Agent{Provider: fake.Script(fake.Call("echo", nil), fake.Text("ok")), Tools: tool.NewRegistry(echo), Hooks: []harness.Hook{denyAll{}, rec}}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil {
		t.Fatal(err)
	}
	tr, _ := rec.Get(res.ID)
	if len(tr.Steps) != 1 || !tr.Steps[0].Denied || tr.Steps[0].Result != "no" {
		t.Fatalf("%+v", tr.Steps)
	}
}

// A trajectory is a view of the journal, so a crash leaves nothing to reassemble.
func TestAJournalIsEnoughAfterACrash(t *testing.T) {
	j := harness.NewMemJournal()
	build := func() *harness.Agent {
		return &harness.Agent{Name: "a", Journal: j, Tools: tool.NewRegistry(echo),
			Provider: fake.NewFunc(func(req model.Request) (model.Response, error) {
				if req.Messages[len(req.Messages)-1].Role == model.RoleTool {
					return fake.Text("ok"), nil
				}
				return fake.Call("echo", map[string]int{"n": 2}), nil
			})}
	}
	in := harness.Prompt("x")
	in.RunID = "r"
	ctx, cancel := context.WithCancel(context.Background())
	stop := func(e harness.Event) {
		if e.Kind == harness.KindToolDone {
			cancel()
		}
	}
	if _, err := build().Run(harness.WithEvents(ctx, stop), in); !errors.Is(err, harness.ErrDrained) {
		t.Fatalf("got %v", err)
	}
	if _, err := build().Run(context.Background(), harness.Resume("r")); err != nil {
		t.Fatal(err)
	}
	evs, err := j.Load(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	r, err := harness.Fold(evs)
	if err != nil {
		t.Fatal(err)
	}
	tr := trajectory.From(*r)
	if tr.Claims != 1 || len(tr.Steps) != 1 || string(tr.Steps[0].Args) != `{"n":2}` || tr.Output != "ok" {
		t.Fatalf("%+v", tr)
	}
}

func TestALostCallIsAStep(t *testing.T) {
	tr := trajectory.From(harness.Run{Events: []harness.Event{{Kind: harness.KindToolLost, Tool: "pay"}}})
	if len(tr.Steps) != 1 || tr.Steps[0].Outcome != "lost" {
		t.Fatalf("%+v", tr.Steps)
	}
}

// AfterRun is at least once, so a repeat must not hide the later record.
func TestGetFindsTheLatestRecord(t *testing.T) {
	rec := trajectory.New()
	rec.AfterRun(context.Background(), harness.Run{ID: "r", Output: "first"})
	rec.AfterRun(context.Background(), harness.Run{ID: "r", Output: "second"})
	if tr, ok := rec.Get("r"); !ok || tr.Output != "second" {
		t.Fatalf("%+v", tr)
	}
}
