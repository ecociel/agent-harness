package profile_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/profile"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func TestProfileHidesToolsAndAddsInstructions(t *testing.T) {
	noop := func(n string) tool.Tool {
		return tool.Func(tool.Spec{Name: n}, func(context.Context, json.RawMessage) (string, error) { return "", nil })
	}
	p := fake.Script(fake.Text("ok"))
	a := &harness.Agent{Model: "claude-x", Instructions: "base", Provider: p,
		Tools: tool.NewRegistry(noop("str_replace"), noop("apply_patch")),
		Hooks: []harness.Hook{profile.Profiles{
			{Match: "gpt", Hide: []string{"str_replace"}},
			{Match: "claude", Hide: []string{"apply_patch"}, Instructions: "Prefer str_replace."},
		}}}
	if _, err := a.Run(context.Background(), harness.Prompt("edit")); err != nil {
		t.Fatal(err)
	}
	req := p.Requests()[0]
	if len(req.Tools) != 1 || req.Tools[0].Name != "str_replace" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if req.Messages[0].Content != "base\n\nPrefer str_replace." {
		t.Fatalf("system = %q", req.Messages[0].Content)
	}
}
