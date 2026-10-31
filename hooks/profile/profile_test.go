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

// Hiding is enough: the core refuses a tool that was not offered, with no hook.
func TestAHiddenToolIsDeniedIfCalledAnyway(t *testing.T) {
	ran := 0
	hidden := tool.Func(tool.Spec{Name: "apply_patch"}, func(context.Context, json.RawMessage) (string, error) {
		ran++
		return "", nil
	})
	p := fake.Script(fake.Call("apply_patch", nil), fake.Text("ok"))
	a := &harness.Agent{Model: "claude-x", Provider: p, Tools: tool.NewRegistry(hidden),
		Hooks: []harness.Hook{profile.Profiles{{Match: "claude", Hide: []string{"apply_patch"}}}}}
	if res, err := a.Run(context.Background(), harness.Prompt("x")); err != nil || !res.OK() || ran != 0 {
		t.Fatalf("%v %+v ran=%d", err, res, ran)
	}
}

func TestAMaxTokensOverrideAndNoMatch(t *testing.T) {
	p := fake.Script(fake.Text("ok"), fake.Text("ok"))
	ps := profile.Profiles{{Match: "small", MaxTokens: 100}}
	for name, want := range map[string]int{"small-1": 100, "large-1": 0} {
		p = fake.Script(fake.Text("ok"))
		a := &harness.Agent{Model: name, Provider: p, Hooks: []harness.Hook{ps}}
		if _, err := a.Run(context.Background(), harness.Prompt("x")); err != nil {
			t.Fatal(err)
		}
		if got := p.Requests()[0].MaxTokens; got != want {
			t.Fatalf("%s: %d", name, got)
		}
	}
}
