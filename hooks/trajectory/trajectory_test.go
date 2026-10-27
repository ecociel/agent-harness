package trajectory_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/trajectory"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func TestRecords(t *testing.T) {
	rec := trajectory.New()
	echo := tool.Func(tool.Spec{Name: "echo"}, func(_ context.Context, a json.RawMessage) (string, error) { return string(a), nil })
	a := &harness.Agent{Provider: fake.Script(fake.Call("echo", map[string]int{"n": 1}), fake.Text("ok")),
		Tools: tool.NewRegistry(echo), Hooks: []harness.Hook{rec}}
	res, _ := a.Run(context.Background(), harness.Prompt("x"))
	tr, ok := rec.Get(res.RunID)
	if !ok || len(tr.Steps) != 1 || tr.Steps[0].Result != `{"n":1}` || tr.Result.Output != "ok" {
		t.Fatalf("%+v", tr)
	}
}
