// Command redact shows the Redact point. It is a ring, not a step: it wraps the provider
// and the tools directly, and every durable event passes through it again. Here an AHV
// number in the prompt and in a tool result never reaches the model, the tool or the
// journal. Kind says where each text is going. The journal is read back as proof.
//
//	go run ./examples/hooks/points/redact
//	HARNESS_PROVIDER=anthropic ANTHROPIC_API_KEY=… go run ./examples/hooks/points/redact
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/hooks/internal/demo"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

const secret = "756.1234.5678.97"

var ahv = regexp.MustCompile(`756\.\d{4}\.\d{4}\.\d{2}`)

// redactor prints each Kind once, because Redact runs on every string of every event.
// It may be called concurrently.
type redactor struct {
	mu   sync.Mutex
	seen map[harness.Kind]bool
}

func (*redactor) Name() string { return "redact-ahv" }

func (h *redactor) Redact(_ context.Context, kind harness.Kind, text string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.seen[kind] {
		h.seen[kind] = true
		fmt.Printf("[redact-ahv] first text of kind %s\n", kind)
	}
	return ahv.ReplaceAllString(text, "[AHV]"), nil
}

func main() {
	fmt.Println("Watch [AHV] replace the number everywhere, then the count of raw copies in the journal.")
	// The mock sees [AHV] like a real model would, so that is what it repeats.
	mock := fake.Script(
		fake.Call("find_customer", map[string]string{"ahv": "[AHV]"}),
		fake.Text("I found Jan, and the record matches [AHV]."),
	)
	provider, modelName := demo.Provider(mock)
	find := tool.Func(tool.Spec{
		Name:        "find_customer",
		Description: "Find a customer by AHV number.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"ahv":{"type":"string"}}}`),
		Effect:      tool.EffectRead,
	}, func(_ context.Context, args json.RawMessage) (string, error) {
		fmt.Printf("[find_customer] got %s\n", args)
		return `{"name":"Jan","ahv":"` + secret + `"}`, nil
	})
	journal := harness.NewMemJournal()
	agent := &harness.Agent{
		Name:         "records",
		Instructions: "Use your tools.",
		Provider:     provider,
		Model:        modelName,
		Tools:        tool.NewRegistry(find),
		Journal:      journal,
		Hooks:        []harness.Hook{&redactor{seen: map[harness.Kind]bool{}}},
	}
	ctx := demo.Context()
	res := demo.Run(ctx, agent, harness.Prompt("Find the customer with AHV number "+secret+"."))
	if sent := mock.Requests(); len(sent) > 0 {
		fmt.Printf("the model was sent: %s\n", model.LastUser(sent[0].Messages))
	}
	events, err := journal.Load(ctx, res.ID)
	if err != nil {
		panic(err)
	}
	raw := 0
	for _, e := range events {
		if doc, _ := json.Marshal(e); strings.Contains(string(doc), secret) {
			raw++
		}
	}
	fmt.Printf("journal: %d events, %d hold the raw number\n", len(events), raw)
}
