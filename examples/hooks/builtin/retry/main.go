// Command retry shows hooks/retry. Every attempt and every backoff is a recorded
// decision, so a crash in the middle of a backoff loses nothing. After the last attempt
// FallbackModel gets one try, and retry.Fallback routes that name to another provider.
// The backoff has jitter, so this example uses a tiny Base and prints no delays.
//
//	go run ./examples/hooks/builtin/retry
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/builtin/retry
package main

import (
	"context"
	"fmt"
	"iter"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/hooks/retry"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
)

func main() {
	fmt.Println("Watch the primary fail twice, then one try on the fallback.")
	provider, modelName := demo.Provider(fake.Script(fake.Text("Bern.")))
	// The fallback is the same provider here, so this works with any one model configured.
	backup := model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
		fmt.Printf("[backup] answering a call for %s\n", req.Model)
		req.Model = modelName
		return provider.Complete(ctx, req)
	})
	agent := &harness.Agent{
		Name:         "geo",
		Instructions: "Answer in one word.",
		Provider:     retry.Fallback(demo.FailFirst(provider, 2, model.ClassTransient), backup, "backup-model"),
		Model:        modelName,
		Hooks:        []harness.Hook{&retry.Retry{Attempts: 2, Base: time.Millisecond, FallbackModel: "backup-model"}},
	}
	demo.Run(demo.Context(), agent, harness.Prompt("What is the capital of Switzerland?"))
}
