package provider_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/anthropic"
	"github.com/ecociel/agent-harness/provider/openai"
	"github.com/ecociel/agent-harness/tool"
)

// Everything else here runs against a script of what the API is documented to send.
// This asks the real one, and it costs money, so it needs saying twice:
//
//	AGENT_HARNESS_LIVE=1 ANTHROPIC_API_KEY=... go test ./provider -run Live -v
//
// The tool turn is the point. A second request that replays the first reply's thinking
// passes only if the signature still matches the history as sent, and a mismatch shows
// up as the extra request in the log the provider writes, not as a failure here.
func TestLiveToolLoop(t *testing.T) {
	if os.Getenv("AGENT_HARNESS_LIVE") == "" {
		t.Skip("set AGENT_HARNESS_LIVE=1 to call the real providers")
	}
	clock := tool.Func(tool.Spec{Name: "clock", Description: "Returns the time.", Effect: tool.EffectRead},
		func(context.Context, json.RawMessage) (string, error) { return "12:00", nil })
	providers := map[string]func() model.Provider{}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		providers["anthropic"] = func() model.Provider {
			p := anthropic.FromEnv()
			if p.Model == "" {
				p.Model = "claude-opus-5-5"
			}
			return p
		}
	}
	if os.Getenv("OPENAI_API_KEY") != "" && os.Getenv("OPENAI_MODEL") != "" {
		providers["openai"] = func() model.Provider { return openai.FromEnv() }
	}
	if len(providers) == 0 {
		t.Skip("no API key set")
	}
	for name, p := range providers {
		t.Run(name, func(t *testing.T) {
			a := &harness.Agent{Name: "live", Provider: p(), Tools: tool.NewRegistry(clock), MaxTokens: 2048,
				Instructions: "Call the clock tool once, then say the time."}
			res, err := a.Run(context.Background(), harness.Prompt("What time is it?"))
			if err != nil || !res.OK() || res.ToolCalls == 0 {
				t.Fatalf("%v %+v", err, res)
			}
			t.Logf("%q, usage %+v", res.Output, res.Usage)
		})
	}
}
