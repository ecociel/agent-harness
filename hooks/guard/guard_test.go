package guard_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/guard"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
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
			rm := tool.Func(tool.Spec{Name: "rm"}, func(context.Context, json.RawMessage) (string, error) { return "gone", nil })
			a := &harness.Agent{Provider: fake.Script(tt.script...), Tools: tool.NewRegistry(rm), Hooks: []harness.Hook{tt.g}}
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

// A Redactor is what keeps a value out of the record. Guard.Redact does not.
func TestRedactorKeepsAValueOutOfTheJournal(t *testing.T) {
	const ahv = "756.1234.5678.97"
	for name, c := range map[string]struct {
		hook harness.Hook
		kept bool
	}{
		"Redactor":     {guard.Redactor(guard.PII()), false},
		"Guard.Redact": {&guard.Guard{Redact: guard.PII()}, true},
	} {
		t.Run(name, func(t *testing.T) {
			j := harness.NewMemJournal()
			a := &harness.Agent{Name: "a", Journal: j, Provider: fake.Script(fake.Text("AHV " + ahv)), Hooks: []harness.Hook{c.hook}}
			in := harness.Prompt("hi")
			in.RunID = "r"
			res, err := a.Run(context.Background(), in)
			if err != nil || res.Output != "AHV [redacted:ahv]" {
				t.Fatalf("%v %+v", err, res)
			}
			evs, _ := j.Load(context.Background(), "r")
			b, _ := json.Marshal(evs)
			if strings.Contains(string(b), ahv) != c.kept {
				t.Fatalf("journal holds it: %v", !c.kept)
			}
		})
	}
}

func TestAToolIsCheckedBeforeItRuns(t *testing.T) {
	ran := 0
	rm := tool.Func(tool.Spec{Name: "rm"}, func(context.Context, json.RawMessage) (string, error) {
		ran++
		return "gone", nil
	})
	for name, g := range map[string]*guard.Guard{
		"not on the allow list": {ToolAllow: []string{"ls"}},
		"on the deny list":      {ToolDeny: []string{"rm"}},
		"arguments":             {ArgDeny: []guard.Rule{guard.R("root", `"/"`)}},
		"a custom check":        {ToolCheck: func(model.ToolCall) error { return errors.New("no") }},
	} {
		t.Run(name, func(t *testing.T) {
			a := &harness.Agent{Tools: tool.NewRegistry(rm), Hooks: []harness.Hook{g},
				Provider: fake.Script(fake.Call("rm", "/"), fake.Text("refused"))}
			res, err := a.Run(context.Background(), harness.Prompt("x"))
			if err != nil || res.Output != "refused" || ran != 0 {
				t.Fatalf("%v %+v ran=%d", err, res, ran)
			}
		})
	}
}

func TestARuleNameIsNotAReplacementPattern(t *testing.T) {
	got, _ := guard.Redactor{guard.R("$0", `secret`)}.Redact(context.Background(), harness.KindToolDone, "a secret")
	if got != "a [redacted:$0]" {
		t.Fatal(got)
	}
}
