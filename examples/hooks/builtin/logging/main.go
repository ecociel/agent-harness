// Command logging shows hooks/logging. Logging is a sink, not a hook: attach it once
// with harness.WithEvents and every run in the tree logs to it, once per event. Only
// what has passed the redactors reaches a sink. A failure, a denial or an early stop
// logs at Warn, and everything else at the level you pass.
//
//	go run ./examples/hooks/builtin/logging
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/builtin/logging
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/hooks/guard"
	"github.com/ecociel/agent-harness/hooks/logging"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func main() {
	fmt.Println("Watch one log line per event, and a WARN for the denied tool. There is no demo timeline here.")
	provider, modelName := demo.Provider(fake.Script(
		fake.Call("delete_user", map[string]int{"id": 42}),
		fake.Text("I am not allowed to delete users."),
	))
	deleteUser := tool.Func(tool.Spec{
		Name:        "delete_user",
		Description: "Delete a user by id.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer"}}}`),
	}, func(context.Context, json.RawMessage) (string, error) { return "deleted", nil })
	agent := &harness.Agent{
		Name:         "ops",
		Instructions: "Use your tools.",
		Provider:     provider,
		Model:        modelName,
		Tools:        tool.NewRegistry(deleteUser),
		Hooks:        []harness.Hook{&guard.Guard{ToolDeny: []string{"delete_user"}}},
	}
	// Times and durations change on every run, and output.txt must not. Keep them in a
	// real logger.
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey || a.Key == "duration" {
				return slog.Attr{}
			}
			return a
		},
	}))
	ctx := harness.WithEvents(context.Background(), logging.Sink(logger, slog.LevelInfo))
	demo.Run(ctx, agent, harness.Prompt("Delete user 42."))
}
