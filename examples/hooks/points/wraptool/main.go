// Command wraptool shows the WrapTool point. A hook wraps one execution, which suits
// offloading, spans or a ledger. c.Key is the call's idempotency key. It stays the same
// across retries and resumes of the call, and the tool reads the same value from
// tool.IdempotencyKey, so an outside system can refuse a repeat.
//
//	go run ./examples/hooks/points/wraptool
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/points/wraptool
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

type ledger struct{}

func (ledger) Name() string { return "ledger" }

func (ledger) WrapTool(_ harness.Run, next harness.ToolFunc) harness.ToolFunc {
	return func(ctx context.Context, c harness.Call) (string, error) {
		fmt.Printf("[ledger] before %s, key %s\n", c.Name, c.Key)
		out, err := next(ctx, c)
		fmt.Printf("[ledger] after %s, the result is %d bytes\n", c.Name, len(out))
		return out, err
	}
}

func main() {
	fmt.Println("Watch the ledger run around the tool, with the key the tool sees too.")
	provider, modelName := demo.Provider(fake.Script(
		fake.Call("send_invoice", map[string]string{"customer": "acme"}),
		fake.Text("The invoice went to acme."),
	))
	send := tool.Func(tool.Spec{
		Name:        "send_invoice",
		Description: "Send the monthly invoice to a customer.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"customer":{"type":"string"}}}`),
		Effect:      tool.EffectIdempotent,
	}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		fmt.Printf("[send_invoice] running with key %s\n", tool.IdempotencyKey(ctx))
		return "sent", nil
	})
	agent := &harness.Agent{
		Name:         "billing",
		Instructions: "Use your tools.",
		Provider:     provider,
		Model:        modelName,
		Tools:        tool.NewRegistry(send),
		Hooks:        []harness.Hook{ledger{}},
	}
	demo.Run(demo.Context(), agent, harness.Prompt("Send acme their monthly invoice."))
}
