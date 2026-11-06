// Package eval scores agents. It has two halves.
//
// The single-turn half runs a Runner against Tasks: an instruction and a Check
// that scores the result, by code, a string match or another agent as judge
// (Run, Contains, StoppedWith, Judge). Report tracks accuracy with tokens and
// latency, because a change that is more accurate but three times as expensive is
// not obviously better.
//
// The multi-turn half plays Scenarios through a consumer's real stack and grades
// the transcript with deterministic checks. A Scenario is a list of Turns, each a
// user message with optional attachments, something for the Target to do first,
// and the answers its waits get, followed by Checks and a Selftest. The library
// takes Go values: the consumer decodes its own files, YAML or anything else, into
// them.
//
// The consumer implements Target over its stack: Open starts a conversation, Send
// plays one turn and answers the waits the turn covers, Facts reports what it saw
// outside the runs (rows written, mail sent). Play builds one Exchange per turn from
// the run with Observe: the answer, every tool call the model asked for with its
// outcome, every wait with its answer, the stop reason, the spend and the latency.
// RunScenarios grades many scenarios into a ScenarioReport, which renders
// summary.md and report.json. A scenario that breaks is an error, not a failure, so
// a broken fixture never counts against the model.
//
// Check kinds live in a registry. Generic holds the kinds that read only the runs.
// A consumer adds kinds that read its Facts, and can register a library kind
// under the name its scenario files already use:
//
//	kinds := eval.Generic()
//	kinds["approval_for"] = kinds["waited_for"]
//	kinds["tasks_created"] = eval.Kind{Shape: eval.Count, Check: func(o eval.Observation, s eval.Spec) string {
//		if n, _ := o.Facts["tasks"].(int); n != s.N {
//			return fmt.Sprintf("%d tasks created, want %d", n, s.N)
//		}
//		return ""
//	}}
//
// A Target over the consumer's stack answers waits the way its product does. Over
// a harness.Session it is a loop:
//
//	func (c *conv) Send(ctx context.Context, t eval.Turn) (*harness.Run, error) {
//		r, err := c.session.Send(ctx, t.Say)
//		for err == nil && r.Stop == harness.StopSuspended {
//			var answers []harness.Signal
//			for _, w := range eval.Observe(r).Waits {
//				if v, ok := t.AnswerFor(w.Tool); ok && w.Open() {
//					answers = append(answers, harness.Signal{Wait: w.ID, Value: v})
//				}
//			}
//			if len(answers) == 0 {
//				break
//			}
//			r, err = c.session.Resume(ctx, answers...)
//		}
//		return r, err
//	}
//
// SelfTest keeps the checks honest. Each scenario carries scripts for a fake model:
// every Good script must pass every check, each Bad script must fail exactly the
// kinds it names, and every kind in use must be the only failure of some Bad
// script. Script.Provider turns a script into a fake model, and SelfTest hands each
// script to the consumer's open function to build a Target around it.
//
// Redaction of the report is the consumer's: Write and Redacted take its rules.
package eval
