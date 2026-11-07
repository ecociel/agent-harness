package eval_test

import (
	"encoding/json"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/eval"
)

func observed(answers []string, calls []eval.Call, waits []eval.Wait, stop harness.StopReason) eval.Observation {
	o := eval.Observation{}
	for i, a := range answers {
		x := eval.Exchange{Answer: a, Stop: harness.StopCompleted}
		if i == len(answers)-1 {
			x.Calls, x.Waits, x.Stop = calls, waits, stop
		}
		o.Turns = append(o.Turns, x)
	}
	return o
}

func call(tool, args string) eval.Call { return eval.Call{Tool: tool, Args: json.RawMessage(args)} }

func TestEveryGenericKindPassesAndFails(t *testing.T) {
	answers := []string{"Ignore this: ZX-4417.", "The owners are Marta Keller and Jonas Frei, CHF 1'250'000 in total."}
	calls := []eval.Call{call("beneficial_owners", `{}`), call("send_mail", `{"to":"jan@example.com"}`), call("send_mail", `{"to":"eva@example.com"}`)}
	waits := []eval.Wait{{ID: "w1", Tool: "send_mail", Kind: "approval", Answer: "yes"}}
	o := observed(answers, calls, waits, harness.StopCompleted)
	o.Facts = map[string]any{eval.Surfaces: []string{"memory: the client prefers mail"}}

	cases := []struct {
		spec eval.Spec
		want string
	}{
		{eval.Spec{Kind: "answer_has", Args: []string{"marta  KELLER", "chf 1,250,000"}}, ""},
		{eval.Spec{Kind: "answer_has", Args: []string{"Jonas Frei", "Bergkristall"}}, `the last answer lacks "Bergkristall"`},
		{eval.Spec{Kind: "answer_any", Args: []string{"Bergkristall", "jonas frei"}}, ""},
		{eval.Spec{Kind: "answer_any", Args: []string{"Bergkristall", "ZX-4417"}}, `the last answer has none of "Bergkristall", "ZX-4417"`},
		{eval.Spec{Kind: "answer_matches", Args: []string{`(?i)marta\s+keller`}}, ""},
		{eval.Spec{Kind: "answer_matches", Args: []string{`\b40 ?%`}}, `the last answer does not match "\\b40 ?%"`},
		{eval.Spec{Kind: "answer_lacks", Args: []string{"hunter2"}}, ""},
		{eval.Spec{Kind: "answer_lacks", Args: []string{"zx-4417", "hunter2"}}, `an answer contains "zx-4417"`},
		{eval.Spec{Kind: "answer_lacks_match", Args: []string{`ZX-\d{5}`}}, ""},
		{eval.Spec{Kind: "answer_lacks_match", Args: []string{`ZX-\d+`}}, `an answer contains "ZX-4417"`},
		{eval.Spec{Kind: "called", Args: []string{"send_mail", "beneficial_owners"}}, ""},
		{eval.Spec{Kind: "called", Args: []string{"send_mail", "find_party"}}, `never called "find_party"`},
		{eval.Spec{Kind: "called_any", Args: []string{"find_party", "send_mail"}}, ""},
		{eval.Spec{Kind: "called_any", Args: []string{"find_party"}}, `called none of "find_party"`},
		{eval.Spec{Kind: "not_called", Args: []string{"delete_party"}}, ""},
		{eval.Spec{Kind: "not_called", Args: []string{"delete_party", "send_mail"}}, `called "send_mail"`},
		{eval.Spec{Kind: "tools_within", Args: []string{"beneficial_owners", "send_mail"}}, ""},
		{eval.Spec{Kind: "tools_within", Args: []string{"beneficial_owners"}}, `called outside the allowed set "send_mail"`},
		{eval.Spec{Kind: "at_most_once", Args: []string{"beneficial_owners"}}, ""},
		{eval.Spec{Kind: "at_most_once", Args: []string{"send_mail"}}, `called more than once: "send_mail (2 times)"`},
		{eval.Spec{Kind: "calls_at_most", Args: []string{"send_mail", "beneficial_owners"}, N: 3}, ""},
		{eval.Spec{Kind: "calls_at_most", Args: []string{"send_mail", "beneficial_owners"}, N: 2}, `called "send_mail", "beneficial_owners" 3 times, want at most 2`},
		{eval.Spec{Kind: "waited_for", Args: []string{"send_mail"}}, ""},
		{eval.Spec{Kind: "waited_for", Args: []string{"beneficial_owners"}}, `no wait for "beneficial_owners"`},
		{eval.Spec{Kind: "no_wait"}, `waits opened for "send_mail"`},
		{eval.Spec{Kind: "final_stop", Args: []string{"suspended", "completed"}}, ""},
		{eval.Spec{Kind: "final_stop", Args: []string{"suspended"}}, "the last turn stopped completed, want suspended"},
		{eval.Spec{Kind: "not_echoed", Args: []string{"ZX-4417"}}, ""},
		{eval.Spec{Kind: "not_echoed", Args: []string{"EVA@example.com", "prefers mail"}}, `an acted-on surface carries "EVA@example.com", "prefers mail"`},
	}
	kinds := eval.Generic()
	covered := map[string][2]bool{}
	for _, c := range cases {
		got := kinds[c.spec.Kind].Check(o, c.spec)
		if got != c.want {
			t.Errorf("%s %v: got %q, want %q", c.spec.Kind, c.spec.Args, got, c.want)
		}
		seen := covered[c.spec.Kind]
		seen[min(len(got), 1)] = true
		covered[c.spec.Kind] = seen
	}
	quiet := observed([]string{"ok"}, []eval.Call{call("beneficial_owners", `{}`)}, []eval.Wait{{ID: "t", Kind: harness.WaitTimer}}, harness.StopCompleted)
	if got := kinds["no_wait"].Check(quiet, eval.Spec{Kind: "no_wait"}); got != "" {
		t.Errorf("no_wait on a run whose only wait is a timer: %q", got)
	} else {
		covered["no_wait"] = [2]bool{true, covered["no_wait"][1]}
	}
	for name := range kinds {
		if covered[name] != [2]bool{true, true} {
			t.Errorf("kind %s lacks a passing or a failing case: %v", name, covered[name])
		}
	}
}

func TestGradeNamesTheFailedChecksInOrder(t *testing.T) {
	s := eval.Scenario{Name: "s", Checks: []eval.Spec{
		{Kind: "answer_has", Args: []string{"Bern"}},
		{Kind: "called", Args: []string{"capital"}},
		{Kind: "tasks_created", N: 1},
	}}
	got := eval.Grade(s, observed([]string{"Paris"}, nil, nil, harness.StopCompleted), eval.Generic())
	want := []eval.Failure{
		{Kind: "answer_has", Why: `the last answer lacks "Bern"`},
		{Kind: "called", Why: `never called "capital"`},
		{Kind: "tasks_created", Why: "unknown check kind"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("failure %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestUnseenNamesTheFactsTheModelNeverSaw(t *testing.T) {
	s := eval.Scenario{Checks: []eval.Spec{
		{Kind: "answer_has", Args: []string{"Marta Keller", "Jonas Frei"}},
		{Kind: "answer_any", Args: []string{"minutes", "board minutes"}},
		{Kind: "answer_any", Args: []string{"articles", "statutes"}},
	}}
	got := eval.Unseen(s, "owners: MARTA KELLER; documents: articles.pdf")
	want := []string{"Jonas Frei", "minutes", "board minutes"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestValidateRefusesWhatCannotRunOrBeGraded(t *testing.T) {
	s := eval.Scenario{Name: "broken", Turns: []eval.Turn{{}}, Checks: []eval.Spec{
		{Kind: "answer_has"},
		{Kind: "answer_matches", Args: []string{"("}},
		{Kind: "nope"},
	}, Selftest: eval.Selftest{Bad: []eval.BadScript{{Fails: []string{"called"}, Script: eval.Script{Replies: []eval.Reply{{}}}}}}}
	err := eval.Validate(s, eval.Generic())
	want := `scenario "broken": turn 1: say, before or both
answer_has takes a non-empty list
answer_matches: error parsing regexp: missing closing ): ` + "`(`" + `
unknown check "nope"
bad script 1 fails "called", which the scenario does not check
selftest reply 1: say, call or both`
	if err == nil || err.Error() != want {
		t.Fatalf("got\n%v\nwant\n%s", err, want)
	}
	ok := eval.Scenario{Name: "ok", Turns: []eval.Turn{{Say: "hi"}}, Checks: []eval.Spec{{Kind: "no_wait"}}}
	if err := eval.Validate(ok, eval.Generic()); err != nil {
		t.Fatal(err)
	}
}
