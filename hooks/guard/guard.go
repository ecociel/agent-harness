// Package guard implements guardrails at the three places the harness can
// veto: the input (BeforeRun), each tool call (BeforeTool) and the final
// output (CheckOutput).
//
// Rules are plain regular expressions with a name, so a violation can be
// explained in one line. Input violations stop the run before any model call
// is made. Tool violations are denied (the model is told why and may adapt)
// or, with HaltOnTool, stop the run. Output violations ask the model to
// revise, or block the answer with BlockOutput. Redact rules rewrite the
// accepted output instead of rejecting it.
//
// Guard.Redact changes only the final answer, so the value stays in the history and
// the journal. To keep a value out of the model, the tools, the hooks and the journal,
// attach a Redactor.
package guard

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
)

// Rule is one named pattern.
type Rule struct {
	Name    string
	Pattern *regexp.Regexp
}

// R builds a Rule, compiling pattern (panics on a bad pattern, like
// regexp.MustCompile).
func R(name, pattern string) Rule { return Rule{Name: name, Pattern: regexp.MustCompile(pattern)} }

// PII returns rules for common personal data: e-mail addresses, payment card
// numbers, IBANs, Swiss AHV numbers and Swiss phone numbers.
func PII() []Rule {
	return []Rule{
		R("email", `[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`),
		// IBAN and AHV come before card so their digits are not taken for a
		// card number (rules are applied in order).
		R("iban", `\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){3,7}(?: ?[A-Z0-9]{1,3})?\b`),
		R("ahv", `\b756\.\d{4}\.\d{4}\.\d{2}\b`),
		// Swiss numbers only, in their grouped forms (+41 79 123 45 67, 079 123 45 67),
		// so an ordinary digit string is not taken for a phone number. Before card.
		R("phone", `(?:\+41|\b0041) ?\(?0?\)?\d{2} ?\d{3} ?\d{2} ?\d{2}\b|\b0\d{2} \d{3} \d{2} \d{2}\b`),
		R("card", `\b(?:\d[ -]?){13,16}\b`),
	}
}

// Secrets returns rules for credentials that must never reach a model or a
// user: API keys, private keys and password assignments.
func Secrets() []Rule {
	return []Rule{
		R("api_key", `\b(?:sk|pk|rk)-[A-Za-z0-9_-]{16,}\b`),
		R("private_key", `-----BEGIN [A-Z ]*PRIVATE KEY-----`),
		R("password", `(?i)\b(?:password|passwort|pwd)\s*(?:[:=]|\bis\b)\s*\S+`),
	}
}

// Guard is the guardrail hook. The zero value allows everything.
type Guard struct {
	// InputDeny rules are matched against the last user message.
	InputDeny []Rule
	// ToolAllow, if non-empty, is the closed set of tools that may run.
	ToolAllow []string
	// ToolDeny names tools that may never run.
	ToolDeny []string
	// ArgDeny rules are matched against raw tool-call arguments.
	ArgDeny []Rule
	// ToolCheck is an optional custom check; a non-nil error denies the call.
	ToolCheck func(call model.ToolCall) error
	// HaltOnTool stops the run on a tool violation instead of denying it.
	HaltOnTool bool
	// OutputDeny rules are matched against the candidate answer.
	OutputDeny []Rule
	// BlockOutput blocks (stops) instead of asking for a revision.
	BlockOutput bool
	// Redact rules are replaced by "[redacted:<name>]" in accepted output.
	Redact []Rule
}

// Name implements harness.Hook.
func (g *Guard) Name() string { return "guard" }

// Redactor replaces every match with "[redacted:<name>]" in everything the run sees
// and records. A match inside provider state, such as a signature that happens to hold
// a long number, invalidates it, and the provider adapter recovers.
type Redactor []Rule

// Name implements harness.Hook.
func (Redactor) Name() string { return "guard.redactor" }

// Redact implements harness.Redactor.
func (rules Redactor) Redact(_ context.Context, _ harness.Kind, text string) (string, error) {
	return redact(rules, text), nil
}

func redact(rules []Rule, s string) string {
	for _, r := range rules {
		s = r.Pattern.ReplaceAllLiteralString(s, "[redacted:"+r.Name+"]")
	}
	return s
}

// BeforeRun checks the input.
func (g *Guard) BeforeRun(_ context.Context, r harness.Run) error {
	if rule, ok := match(g.InputDeny, model.LastUser(r.Messages)); ok {
		return harness.Stop(harness.StopGuardrail, "input matches rule "+rule)
	}
	return nil
}

// BeforeTool checks one tool call.
func (g *Guard) BeforeTool(_ context.Context, _ harness.Run, c harness.Call) harness.Decision {
	reason := g.toolViolation(c.ToolCall)
	switch {
	case reason == "":
		return harness.Allow()
	case g.HaltOnTool:
		return harness.Halt(reason)
	default:
		return harness.Deny(reason)
	}
}

func (g *Guard) toolViolation(call model.ToolCall) string {
	if len(g.ToolAllow) > 0 && !slices.Contains(g.ToolAllow, call.Name) {
		return fmt.Sprintf("tool %q is not on the allow list", call.Name)
	}
	if slices.Contains(g.ToolDeny, call.Name) {
		return fmt.Sprintf("tool %q is denied by policy", call.Name)
	}
	if rule, ok := match(g.ArgDeny, string(call.Arguments)); ok {
		return fmt.Sprintf("arguments match guard rule %s", rule)
	}
	if g.ToolCheck != nil {
		if err := g.ToolCheck(call); err != nil {
			return err.Error()
		}
	}
	return ""
}

// CheckOutput checks and redacts the candidate answer.
func (g *Guard) CheckOutput(ctx context.Context, _ harness.Run, output string) harness.Verdict {
	if rule, ok := match(g.OutputDeny, output); ok {
		msg := "the answer contains content matching rule " + rule
		if g.BlockOutput {
			return harness.Block(msg)
		}
		return harness.Revise(msg + "; remove it")
	}
	if redacted := redact(g.Redact, output); redacted != output {
		harness.Note(ctx, "redacted %s", strings.Join(redactedNames(g.Redact, output), ", "))
		return harness.Verdict{Rewrite: redacted}
	}
	return harness.Accept()
}

func match(rules []Rule, s string) (string, bool) {
	for _, r := range rules {
		if r.Pattern.MatchString(s) {
			return r.Name, true
		}
	}
	return "", false
}

func redactedNames(rules []Rule, s string) []string {
	var names []string
	for _, r := range rules {
		if r.Pattern.MatchString(s) {
			names = append(names, r.Name)
		}
	}
	return names
}
