// Command beforetool shows the BeforeTool point. A hook decides each tool call before
// it runs. Allow lets it run, Deny tells the model why and lets it adapt, and Halt
// ends the run. Every decision is recorded.
//
//	go run ./examples/hooks/points/beforetool
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/points/beforetool
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

type policy struct{}

func (policy) Name() string { return "policy" }

func (policy) BeforeTool(_ context.Context, _ harness.Run, c harness.Call) harness.Decision {
	switch c.Name {
	case "read_file":
		return harness.Allow()
	case "delete_file":
		fmt.Println("[policy] deleting is not allowed, so tell the model")
		return harness.Deny("this workspace is read-only")
	}
	fmt.Printf("[policy] %s is never acceptable, so end the run\n", c.Name)
	return harness.Halt("attempted to wipe the disk")
}

func stub(name, description, result string) tool.Tool {
	path := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
	return tool.Func(tool.Spec{Name: name, Description: description, Parameters: path},
		func(context.Context, json.RawMessage) (string, error) { return result, nil })
}

func main() {
	fmt.Println("Watch the gate lines: one call runs, one is denied and the model adapts, one halts the run.")
	provider, modelName := demo.Provider(fake.Script(
		fake.Call("read_file", map[string]string{"path": "notes.txt"}),
		fake.Call("delete_file", map[string]string{"path": "notes.txt"}),
		fake.Text("notes.txt says to buy milk. I could not delete it, the workspace is read-only."),
		fake.Call("wipe_disk", struct{}{}),
	))
	agent := &harness.Agent{
		Name:         "files",
		Instructions: "You manage files with the tools you are given.",
		Provider:     provider,
		Model:        modelName,
		Tools: tool.NewRegistry(
			stub("read_file", "Read a file.", "buy milk"),
			stub("delete_file", "Delete a file.", "deleted"),
			stub("wipe_disk", "Erase every file on the disk.", "wiped"),
		),
		Hooks: []harness.Hook{policy{}},
	}
	ctx := demo.Context()
	demo.Run(ctx, agent, harness.Prompt("Read notes.txt, tell me what it says, then delete it."))
	demo.Run(ctx, agent, harness.Prompt("Wipe the disk."))
}
