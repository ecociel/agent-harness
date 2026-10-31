// Command tour is the place to start. One run passes through all 12 plugin points and
// each prints a line as it fires. It takes two hooks: a redactor that is also model
// middleware moves inside the redaction ring and leaves WrapModel, so Redact has a type
// of its own.
//
//	go run ./examples/hooks/tour
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/tour
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"sync"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

// watcher implements 11 points and only reports that they fired.
type watcher struct{}

func (watcher) Name() string { return "watcher" }

func (watcher) BeforeRun(context.Context, harness.Run) error {
	fmt.Println("[BeforeRun] a worker took the run, admit it or Stop it")
	return nil
}

func (watcher) Context(_ context.Context, r harness.Run) (harness.Edit, error) {
	fmt.Printf("[Context] the history has %d, a recorded Edit could change it\n", len(r.Messages))
	return harness.Edit{}, nil
}

func (watcher) BeforeModel(_ context.Context, _ harness.Run, req *model.Request) error {
	fmt.Printf("[BeforeModel] may change this request, %d messages with the system one\n", len(req.Messages))
	return nil
}

func (watcher) WrapModel(_ harness.Run, next model.Provider) model.Provider {
	return model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
		fmt.Println("[WrapModel] the stream passes through here")
		return next.Complete(ctx, req)
	})
}

func (watcher) Recover(_ context.Context, _ harness.Run, f harness.Failure) harness.Recovery {
	fmt.Printf("[Recover] %s failure, retry it\n", f.Class)
	return harness.Recovery{Retry: true, After: time.Millisecond}
}

func (watcher) AfterModel(_ context.Context, _ harness.Run, resp model.Response) (model.Response, error) {
	fmt.Println("[AfterModel] may revise the reply before it is recorded")
	return resp, nil
}

func (watcher) BeforeTool(_ context.Context, _ harness.Run, c harness.Call) harness.Decision {
	fmt.Printf("[BeforeTool] allow %s\n", c.Name)
	return harness.Allow()
}

func (watcher) WrapTool(_ harness.Run, next harness.ToolFunc) harness.ToolFunc {
	return func(ctx context.Context, c harness.Call) (string, error) {
		fmt.Printf("[WrapTool] around %s\n", c.Name)
		return next(ctx, c)
	}
}

func (watcher) AfterTool(_ context.Context, _ harness.Run, c harness.Call, content string) (string, error) {
	fmt.Printf("[AfterTool] may revise the result of %s\n", c.Name)
	return content, nil
}

func (watcher) CheckOutput(context.Context, harness.Run, string) harness.Verdict {
	fmt.Println("[CheckOutput] accept the answer")
	return harness.Accept()
}

func (watcher) AfterRun(_ context.Context, r harness.Run) {
	fmt.Printf("[AfterRun] the outcome is %s\n", r.Stop)
}

// announcer is the Redactor. Redact runs on every text of every durable event, so it
// prints once per Kind. It may be called concurrently.
type announcer struct {
	mu   sync.Mutex
	seen map[harness.Kind]bool
}

func (*announcer) Name() string { return "announcer" }

func (a *announcer) Redact(_ context.Context, kind harness.Kind, text string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.seen[kind] {
		a.seen[kind] = true
		fmt.Printf("[Redact] first text of kind %s\n", kind)
	}
	return text, nil
}

func main() {
	fmt.Println("Follow the [Point] lines: one failed call, one tool call, one answer.")
	provider, modelName := demo.Provider(fake.Script(
		fake.Call("weather", map[string]string{"city": "Zurich"}),
		fake.Text("It is 11 °C in Zurich."),
	))
	weather := tool.Func(tool.Spec{
		Name:        "weather",
		Description: "Current weather for a city.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		Effect:      tool.EffectRead,
	}, func(context.Context, json.RawMessage) (string, error) { return `{"temp_c":11}`, nil })
	agent := &harness.Agent{
		Name:         "tour",
		Instructions: "Answer briefly. Use the weather tool.",
		Provider:     demo.FailFirst(provider, 1, model.ClassTransient),
		Model:        modelName,
		Tools:        tool.NewRegistry(weather),
		Hooks:        []harness.Hook{&announcer{seen: map[harness.Kind]bool{}}, watcher{}},
	}
	demo.Run(demo.Context(), agent, harness.Prompt("What is the weather in Zurich?"))
}
