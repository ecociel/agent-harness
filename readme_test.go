package harness_test

import (
	"context"
	"encoding/json"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/budget"
	"github.com/ecociel/agent-harness/hooks/compact"
	"github.com/ecociel/agent-harness/hooks/guard"
	"github.com/ecociel/agent-harness/hooks/metrics"
	"github.com/ecociel/agent-harness/hooks/retry"
	"github.com/ecociel/agent-harness/provider/anthropic"
	"github.com/ecociel/agent-harness/tool"
)

// This is the README's first example. It has no Output line, so it compiles and does
// not run: it needs a key. If it stops compiling, the README is wrong.
func Example_production() {
	ctx := context.Background()
	registry := metrics.NewRegistry()
	weather := tool.Func(tool.Spec{Name: "weather", Description: "Weather for a city.", Effect: tool.EffectRead},
		func(ctx context.Context, args json.RawMessage) (string, error) { return `{"temp_c":11}`, nil })

	a := &harness.Agent{
		Name:         "assistant",
		Instructions: "Answer briefly.",
		Provider:     anthropic.FromEnv(),
		Model:        "claude-opus-5-5",
		MaxTokens:    16_000,
		Tools:        tool.NewRegistry(weather),
		Hooks: []harness.Hook{
			guard.Redactor(guard.PII()),
			&guard.Guard{InputDeny: guard.Secrets()},
			budget.New(budget.Limits{Tokens: 50_000, RepeatCalls: 3}),
			&compact.Compactor{MaxTokens: 8_000},
			&retry.Retry{Attempts: 3},
		},
	}
	ctx = harness.WithEvents(ctx, metrics.Sink(registry))
	res, err := a.Run(ctx, harness.Prompt("Weather in Zurich?"))
	_, _ = res, err
}
