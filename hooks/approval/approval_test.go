package approval_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/approval"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func TestPolicy(t *testing.T) {
	ran := false
	pay := tool.Func(tool.Spec{Name: "pay", Risk: tool.RiskDangerous, Scope: "payments"},
		func(context.Context, json.RawMessage) (string, error) { ran = true; return "paid", nil })
	tests := []struct {
		name      string
		principal approval.Principal
		approver  approval.Approver
		wantRan   bool
		wantRule  string
	}{
		{"missing scope", approval.Principal{ID: "u"}, nil, false, "scope"},
		{"no approver", approval.Principal{ID: "u", Scopes: []string{"payments"}}, nil, false, "approval"},
		{"rejected", approval.Principal{ID: "u", Scopes: []string{"payments"}},
			approval.ApproverFunc(func(context.Context, approval.Request) (bool, string) { return false, "nope" }), false, "approval"},
		{"approved", approval.Principal{ID: "u", Scopes: []string{"payments"}},
			approval.ApproverFunc(func(context.Context, approval.Request) (bool, string) { return true, "" }), true, "approved"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ran = false
			var audit []approval.Record
			p := approval.New(tool.RiskWrite, tt.approver)
			p.Audit = func(r approval.Record) { audit = append(audit, r) }
			a := &harness.Agent{Provider: fake.Script(fake.Call("pay", nil), fake.Text("done")),
				Tools: tool.NewRegistry(pay), Hooks: []harness.Hook{p}}
			res, err := a.Run(approval.WithPrincipal(context.Background(), tt.principal), harness.Prompt("pay"))
			if err != nil || !res.OK() {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			if ran != tt.wantRan || len(audit) != 1 || audit[0].Rule != tt.wantRule {
				t.Fatalf("ran=%v audit=%+v", ran, audit)
			}
			if !tt.wantRan && !strings.Contains(res.Messages[2].Content, `"denied":true`) {
				t.Fatalf("model not told: %q", res.Messages[2].Content)
			}
		})
	}
}
