package offload_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/offload"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func TestLargeResultIsOffloadedAndReadable(t *testing.T) {
	store := offload.NewStore()
	big := strings.Repeat("x", 10000)
	dump := tool.Func(tool.Spec{Name: "dump"}, func(context.Context, json.RawMessage) (string, error) { return big, nil })
	a := &harness.Agent{
		Provider: fake.Script(fake.Call("dump", nil),
			fake.Call(offload.ReaderName, map[string]any{"id": "off-1", "offset": 9990}), fake.Text("ok")),
		Tools: tool.NewRegistry(dump, offload.Reader(store)),
		Hooks: []harness.Hook{offload.New(store, 1000)},
	}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil || !res.OK() {
		t.Fatalf("%+v %v", res, err)
	}
	if got := res.Messages[2].Content; len(got) > 600 || !strings.Contains(got, `"offloaded":"off-1"`) {
		t.Fatalf("inline result = %d bytes", len(got))
	}
	if got := res.Messages[4].Content; got != strings.Repeat("x", 10) {
		t.Fatalf("read back %q", got)
	}
}
