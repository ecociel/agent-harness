package guard_test

import (
	"context"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/guard"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
)

func TestGuard(t *testing.T) {
	tests := []struct {
		name     string
		g        *guard.Guard
		prompt   string
		script   []model.Response
		wantStop harness.StopReason
		wantOut  string
	}{
		{"input blocked", &guard.Guard{InputDeny: guard.Secrets()}, "my password: hunter2",
			[]model.Response{fake.Text("x")}, harness.StopGuardrail, ""},
		{"tool denied then answer", &guard.Guard{ToolDeny: []string{"rm"}}, "clean up",
			[]model.Response{fake.Call("rm", nil), fake.Text("I may not delete")}, harness.StopCompleted, "I may not delete"},
		{"tool halt", &guard.Guard{ToolDeny: []string{"rm"}, HaltOnTool: true}, "clean up",
			[]model.Response{fake.Call("rm", nil)}, harness.StopGuardrail, ""},
		{"output revised", &guard.Guard{OutputDeny: []guard.Rule{guard.R("swear", `(?i)darn`)}}, "hi",
			[]model.Response{fake.Text("darn"), fake.Text("hello")}, harness.StopCompleted, "hello"},
		{"output blocked", &guard.Guard{OutputDeny: guard.PII(), BlockOutput: true}, "hi",
			[]model.Response{fake.Text("mail me at a@b.ch")}, harness.StopGuardrail, ""},
		{"redacted", &guard.Guard{Redact: guard.PII()}, "hi",
			[]model.Response{fake.Text("AHV 756.1234.5678.97")}, harness.StopCompleted, "AHV [redacted:ahv]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &harness.Agent{Provider: fake.Script(tt.script...), Hooks: []harness.Hook{tt.g}}
			res, err := a.Run(context.Background(), harness.Prompt(tt.prompt))
			if err != nil {
				t.Fatal(err)
			}
			if res.Stop != tt.wantStop || res.Output != tt.wantOut {
				t.Fatalf("stop=%s out=%q detail=%s", res.Stop, res.Output, res.Detail)
			}
		})
	}
}
