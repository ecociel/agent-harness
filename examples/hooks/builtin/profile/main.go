// Command profile shows hooks/profile. Keep the harness general and let each model
// family see the tools and instructions it works best with. The first profile whose
// Match prefixes the model name applies, to one request at a time. Here one family gets
// a string-replace editor and the other a patch tool. show prints what the provider
// is really sent, because it is listed after the profile.
//
//	go run ./examples/hooks/builtin/profile
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/builtin/profile
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/hooks/profile"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

type show struct{}

func (show) Name() string { return "show" }

func (show) BeforeModel(_ context.Context, _ harness.Run, req *model.Request) error {
	fmt.Printf("[show] system message %q, max tokens %d\n", req.Messages[0].Content, req.MaxTokens)
	return nil
}

func editor(name, description string) tool.Tool {
	return tool.Func(tool.Spec{Name: name, Description: description},
		func(context.Context, json.RawMessage) (string, error) { return "edited", nil })
}

func main() {
	fmt.Println("Watch the offered tools: the profile hid one, and added to the system message.")
	provider, modelName := demo.Provider(fake.Script(
		fake.Call("str_replace", map[string]string{"old": "8000", "new": "8080"}),
		fake.Text("The port in config.yaml is now 8080."),
	))
	profiles := profile.Profiles{
		{Match: "gpt", Instructions: "Edit files with apply_patch.", Hide: []string{"str_replace"}},
		{Match: "claude", Instructions: "Edit files with str_replace.", Hide: []string{"apply_patch"}},
		{Match: "mock", Instructions: "Edit files with str_replace.", Hide: []string{"apply_patch"}, MaxTokens: 500},
	}
	agent := &harness.Agent{
		Name:         "editor",
		Instructions: "You edit files.",
		Provider:     provider,
		Model:        modelName,
		Tools: tool.NewRegistry(
			editor("str_replace", "Replace one string in a file."),
			editor("apply_patch", "Apply a unified diff."),
		),
		Hooks: []harness.Hook{profiles, show{}},
	}
	demo.Run(demo.Context(), agent, harness.Prompt("In config.yaml, change the port from 8000 to 8080."))
}
