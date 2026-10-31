// Command offload shows hooks/offload. A tool result over the limit is moved to a Store
// and replaced by a small handle with a preview, so it stops costing tokens on every
// later turn. Nothing is lost: the model pages through the original with the tool from
// offload.Reader. A durable run needs a durable Store.
//
//	go run ./examples/hooks/builtin/offload
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/builtin/offload
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/hooks/offload"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

var buildLog = strings.Repeat("step ok\n", 100) + "error: disk full\n"

// reader acts like a model: it fetches the log, then reads the handle it got back.
func reader(req model.Request) (model.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	switch {
	case last.Role != model.RoleTool:
		return fake.Call("fetch_log", struct{}{}), nil
	case last.Name == offload.ReaderName:
		return fake.Text("The build failed because the disk is full."), nil
	}
	var handle struct {
		ID    string `json:"offloaded"`
		Bytes int    `json:"bytes"`
	}
	if err := json.Unmarshal([]byte(last.Content), &handle); err != nil {
		return model.Response{}, err
	}
	return fake.Call(offload.ReaderName, map[string]any{"id": handle.ID, "offset": handle.Bytes - 33}), nil
}

func main() {
	fmt.Printf("Watch a result of %d bytes come back as a handle, and the model read its tail.\n", len(buildLog))
	provider, modelName := demo.Provider(fake.NewFunc(reader))
	store := offload.NewMemStore()
	fetch := tool.Func(tool.Spec{Name: "fetch_log", Description: "Fetch the latest build log.", Effect: tool.EffectRead},
		func(context.Context, json.RawMessage) (string, error) { return buildLog, nil })
	agent := &harness.Agent{
		Name:         "ci",
		Instructions: "Use fetch_log, and read_offloaded if the log was moved. The end of the log matters most.",
		Provider:     provider,
		Model:        modelName,
		Tools:        tool.NewRegistry(fetch, offload.Reader(store)),
		Hooks:        []harness.Hook{&offload.Offloader{Store: store, Limit: 200, Preview: 16}},
	}
	res := demo.Run(demo.Context(), agent, harness.Prompt("Why did the last build fail?"))
	for _, m := range res.Messages {
		if m.Name == "fetch_log" {
			fmt.Printf("the history keeps %d bytes of the result: %s\n", len(m.Content), m.Content)
		}
	}
}
