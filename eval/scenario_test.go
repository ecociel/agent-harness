package eval_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/eval"
	"github.com/ecociel/agent-harness/hooks/approval"
	"github.com/ecociel/agent-harness/tool"
)

// clerk is a Target over a real harness.Agent with a scripted model: a read tool,
// a mail tool behind an approval, and a Session that carries the history.
type clerk struct{ script *eval.Script }

func (c clerk) Open(_ context.Context, s eval.Scenario) (eval.Conversation, error) {
	script := c.script
	if script == nil {
		script = &s.Selftest.Good[0]
	}
	p, err := script.Provider(map[string]string{"owner_mail": "jan@example.com"})
	if err != nil {
		return nil, err
	}
	conv := &mailbox{}
	owners := tool.Func(tool.Spec{Name: "owners", Effect: tool.EffectRead},
		func(context.Context, json.RawMessage) (string, error) { return `["Marta Keller","Jonas Frei"]`, nil })
	send := tool.Func(tool.Spec{Name: "send_mail", Risk: tool.RiskDangerous},
		func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct{ To string }
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			conv.sent = append(conv.sent, in.To)
			return "sent", nil
		})
	conv.session = harness.NewSession(&harness.Agent{Name: "clerk", Provider: p, Journal: harness.NewMemJournal(),
		Tools: tool.NewRegistry(owners, send), Hooks: []harness.Hook{&approval.Policy{Threshold: tool.RiskDangerous, Ask: true}}})
	return conv, nil
}

type mailbox struct {
	session *harness.Session
	sent    []string
}

var signalOf = map[string]string{"approve": approval.Approved, "reject": "no"}

func (m *mailbox) Send(ctx context.Context, t eval.Turn) (*harness.Run, error) {
	r, err := m.session.Send(ctx, t.Say)
	for err == nil && r.Stop == harness.StopSuspended {
		var answers []harness.Signal
		for _, w := range eval.Observe(r).Waits {
			if v, ok := t.AnswerFor(w.Tool); ok && w.Open() {
				answers = append(answers, harness.Signal{Wait: w.ID, Value: signalOf[v], By: "eval"})
			}
		}
		if len(answers) == 0 {
			break
		}
		r, err = m.session.Resume(ctx, answers...)
	}
	return r, err
}

func (m *mailbox) Facts(context.Context) (map[string]any, error) {
	return map[string]any{"mail": slices.Clone(m.sent)}, nil
}

func (m *mailbox) Close(context.Context) error { return nil }

// kinds are the generic kinds plus one that reads the consumer's facts.
func kinds() eval.Kinds {
	k := eval.Generic()
	k["mail_sent_to"] = eval.Kind{Shape: eval.List, Check: func(o eval.Observation, s eval.Spec) string {
		sent, _ := o.Facts["mail"].([]string)
		var missing []string
		for _, to := range s.Args {
			if !slices.Contains(sent, to) {
				missing = append(missing, to)
			}
		}
		if len(missing) == 0 {
			return ""
		}
		return "no mail was sent to " + strings.Join(missing, ", ")
	}}
	return k
}

func say(text string) eval.Reply               { return eval.Reply{Say: text} }
func use(name, args string) eval.Reply         { return eval.Reply{Call: name, Args: args} }
func script(replies ...eval.Reply) eval.Script { return eval.Script{Replies: replies} }

func mailOwners() eval.Scenario {
	return eval.Scenario{
		Name: "mail-owners", Category: "correctness",
		Turns: []eval.Turn{
			{Say: "Who owns Alpenrose?"},
			{Say: "Mail that to Jan.", Answer: map[string]string{"send_mail": "approve"}},
		},
		Checks: []eval.Spec{
			{Kind: "answer_has", Args: []string{"sent"}},
			{Kind: "called", Args: []string{"owners", "send_mail"}},
			{Kind: "waited_for", Args: []string{"send_mail"}},
			{Kind: "final_stop", Args: []string{"completed"}},
			{Kind: "mail_sent_to", Args: []string{"jan@example.com"}},
		},
		Selftest: eval.Selftest{
			Good: []eval.Script{script(
				use("owners", ""), say("Marta Keller and Jonas Frei."),
				use("send_mail", `{"to":"{{owner_mail}}"}`), say("Sent to Jan."),
			)},
			Bad: []eval.BadScript{
				{Fails: []string{"answer_has"}, Script: script(
					use("owners", ""), say("Marta Keller and Jonas Frei."),
					use("send_mail", `{"to":"{{owner_mail}}"}`), say("Done."),
				)},
				{Fails: []string{"mail_sent_to"}, Script: script(
					use("owners", ""), say("Marta Keller and Jonas Frei."),
					use("send_mail", `{"to":"eva@example.com"}`), say("Sent to Eva."),
				)},
			},
		},
	}
}

func TestAScenarioPlaysThroughARealAgentAndAnswersItsWait(t *testing.T) {
	s := mailOwners()
	o, err := eval.Play(context.Background(), clerk{}, s)
	if err != nil {
		t.Fatal(err)
	}
	if got := o.Answers(); !slices.Equal(got, []string{"Marta Keller and Jonas Frei.", "Sent to Jan."}) {
		t.Errorf("answers %q", got)
	}
	var calls []string
	for _, c := range o.Calls() {
		calls = append(calls, fmt.Sprintf("%s %s %s %s", c.Tool, c.Args, c.Outcome, c.Result))
	}
	want := []string{`owners {} ok ["Marta Keller","Jonas Frei"]`, `send_mail {"to":"jan@example.com"} ok sent`}
	if !slices.Equal(calls, want) {
		t.Errorf("calls\n%q\nwant\n%q", calls, want)
	}
	if w := o.Turns[1].Waits; len(w) != 1 || w[0].Tool != "send_mail" || w[0].Kind != approval.WaitKind || w[0].Answer != approval.Approved || w[0].Open() {
		t.Errorf("waits %+v", w)
	}
	if len(o.Turns[0].Waits) != 0 || o.Turns[1].Stop != harness.StopCompleted || o.Turns[1].Spend.ModelCalls != 2 {
		t.Errorf("turns %+v", o.Turns)
	}
	if f := eval.Grade(s, o, kinds()); len(f) != 0 {
		t.Errorf("failures %v", f)
	}
}

func TestAnOpenWaitEndsTheTurnSuspended(t *testing.T) {
	s := mailOwners()
	s.Turns[1].Answer = nil
	s.Checks = []eval.Spec{{Kind: "final_stop", Args: []string{"completed"}}, {Kind: "mail_sent_to", Args: []string{"jan@example.com"}}}
	o, err := eval.Play(context.Background(), clerk{}, s)
	if err != nil {
		t.Fatal(err)
	}
	if w := o.Turns[1].Waits; len(w) != 1 || !w[0].Open() || w[0].Tool != "send_mail" {
		t.Errorf("waits %+v", w)
	}
	if c := o.Turns[1].Calls; len(c) != 1 || c[0].Outcome != "" {
		t.Errorf("a call still waiting has no outcome: %+v", c)
	}
	got := eval.Grade(s, o, kinds())
	want := []eval.Failure{
		{Kind: "final_stop", Why: "the last turn stopped suspended, want completed"},
		{Kind: "mail_sent_to", Why: "no mail was sent to jan@example.com"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestARejectedWaitIsADenial(t *testing.T) {
	s := mailOwners()
	s.Turns[1].Answer = map[string]string{"*": "reject"}
	o, err := eval.Play(context.Background(), clerk{}, s)
	if err != nil {
		t.Fatal(err)
	}
	c := o.Turns[1].Calls
	if len(c) != 1 || c[0].Outcome != "denied" || c[0].Result != "declined by eval" || o.Turns[1].Waits[0].Answer != "no" {
		t.Errorf("calls %+v waits %+v", c, o.Turns[1].Waits)
	}
}

func TestTheReportCountsPassesFailuresAndErrorsApart(t *testing.T) {
	pass := mailOwners()
	fail := mailOwners()
	fail.Name, fail.Category, fail.Channel = "mail-eva", "injection", "document"
	fail.Selftest.Good = []eval.Script{fail.Selftest.Bad[1].Script}
	broken := mailOwners()
	broken.Name = "no-script-left"
	broken.Selftest.Good = []eval.Script{script(use("owners", `{"x":"{{missing}}"}`))}

	rep := eval.RunScenarios(context.Background(), clerk{}, []eval.Scenario{pass, fail, broken}, kinds(), 2)

	if len(rep.Results) != 3 || !rep.Results[0].Pass || rep.Results[1].Pass || rep.Errors != 1 {
		t.Fatalf("%+v", rep)
	}
	if got := rep.Results[1].Failed; !slices.Equal(got, []eval.Failure{{Kind: "mail_sent_to", Why: "no mail was sent to jan@example.com"}}) {
		t.Errorf("failed %v", got)
	}
	if got := rep.Results[2].Err; got != `open: reply 1: {"x":"{{missing}}"} names a variable that is not set` {
		t.Errorf("error %q", got)
	}
	if rep.Categories["correctness"] != (eval.Tally{Passed: 1, Total: 1}) || rep.Channels["document"] != (eval.Tally{Passed: 0, Total: 1}) {
		t.Errorf("tallies %v %v", rep.Categories, rep.Channels)
	}
	if !slices.Equal(rep.Results[0].Tools, []string{"owners", "send_mail"}) || rep.Spend.ModelCalls != 8 {
		t.Errorf("tools %v spend %+v", rep.Results[0].Tools, rep.Spend)
	}
	md := rep.Markdown()
	for _, line := range []string{
		"Scenarios that broke instead of being graded (left out of the counts): 1.",
		"| correctness | 1 / 1 |",
		"| injection | 0 / 1 |",
		"| document | 0 / 1 |",
		"| mail-eva | FAIL |",
		"| mail_sent_to: no mail was sent to jan@example.com |",
		"| no-script-left | error |",
	} {
		if !strings.Contains(md, line) {
			t.Errorf("summary lacks %q:\n%s", line, md)
		}
	}

	clean, masked := rep.Redacted(func(s string) string { return strings.ReplaceAll(s, "jan@example.com", "[mail]") })
	if masked != 1 || clean.Results[1].Failed[0].Why != "no mail was sent to [mail]" || rep.Results[1].Failed[0].Why != "no mail was sent to jan@example.com" {
		t.Errorf("masked %d: %+v", masked, clean.Results[1].Failed)
	}
}

func scripted(sc eval.Script) eval.Target { return clerk{script: &sc} }

func provenScenario() eval.Scenario {
	s := mailOwners()
	s.Checks = []eval.Spec{
		{Kind: "answer_has", Args: []string{"sent"}},
		{Kind: "called", Args: []string{"owners"}},
		{Kind: "mail_sent_to", Args: []string{"jan@example.com"}},
	}
	s.Selftest.Bad = append(s.Selftest.Bad, eval.BadScript{Fails: []string{"called"}, Script: script(
		say("Marta Keller and Jonas Frei, I believe."),
		use("send_mail", `{"to":"{{owner_mail}}"}`), say("Sent to Jan."),
	)})
	return s
}

func TestSelfTestPassesAScenarioWhoseChecksAreEachProven(t *testing.T) {
	if err := eval.SelfTest(context.Background(), scripted, []eval.Scenario{provenScenario()}, kinds()); err != nil {
		t.Fatal(err)
	}
}

func TestSelfTestRefusesABadScriptThatFailsTheWrongCheck(t *testing.T) {
	s := provenScenario()
	s.Selftest.Bad[0].Fails = []string{"called"}
	err := eval.SelfTest(context.Background(), scripted, []eval.Scenario{s}, kinds())
	want := "mail-owners: bad script 1 fails [answer_has], want [called]\nno bad script fails answer_has alone"
	if err == nil || err.Error() != want {
		t.Fatalf("got\n%v\nwant\n%s", err, want)
	}
}

func TestSelfTestRefusesAGoodScriptThatFailsAndAKindNeverProven(t *testing.T) {
	s := provenScenario()
	s.Selftest.Good[0].Replies[3] = say("Done.")
	s.Checks = append(s.Checks, eval.Spec{Kind: "final_stop", Args: []string{"completed"}})
	err := eval.SelfTest(context.Background(), scripted, []eval.Scenario{s}, kinds())
	want := "mail-owners: good script 1 fails answer_has: the last answer lacks \"sent\"\nno bad script fails final_stop alone"
	if err == nil || err.Error() != want {
		t.Fatalf("got\n%v\nwant\n%s", err, want)
	}
}
