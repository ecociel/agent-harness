package eval

import (
	"errors"
	"fmt"
	"slices"
)

// Scenario is one conversation to run and what must hold afterwards. The library
// takes these as Go values: a consumer decodes its own scenario files into them.
type Scenario struct {
	Name     string `json:"name" yaml:"name"`
	Category string `json:"category,omitempty" yaml:"category,omitempty"` // "correctness", "injection", ...
	// Channel is where an injection scenario plants its instruction.
	Channel  string   `json:"channel,omitempty" yaml:"channel,omitempty"`
	Turns    []Turn   `json:"turns" yaml:"turns"`
	Checks   []Spec   `json:"checks" yaml:"checks"`
	Selftest Selftest `json:"selftest,omitzero" yaml:"selftest,omitempty"`
}

// Turn is one thing that reaches the conversation and how its waits are answered.
type Turn struct {
	// Say is the user's message. A turn with only Before is whatever Before makes
	// happen, such as a scheduled note coming due.
	Say string `json:"say,omitempty" yaml:"say,omitempty"`
	// Attach names files the Target attaches to the message. It resolves them.
	Attach []string `json:"attach,omitempty" yaml:"attach,omitempty"`
	// Before is for the Target to act on before the turn: plant a follow-up, move
	// a clock.
	Before string `json:"before,omitempty" yaml:"before,omitempty"`
	// Answer maps a tool name to what every wait of a call to that tool is answered
	// with, "*" to what every other wait is. The Target says what a value means. A
	// wait with no entry stays open and the turn ends suspended.
	Answer map[string]string `json:"answer,omitempty" yaml:"answer,omitempty"`
}

// AnswerFor returns what a wait of a call to tool is answered with, if anything.
func (t Turn) AnswerFor(tool string) (string, bool) {
	if v, ok := t.Answer[tool]; ok {
		return v, true
	}
	v, ok := t.Answer["*"]
	return v, ok
}

// Selftest is the good and bad discipline that proves a scenario's checks tell
// the two apart. Every Good script must pass every check, and each Bad script must
// fail exactly the kinds in its Fails.
type Selftest struct {
	Good []Script    `json:"good,omitempty" yaml:"good,omitempty"`
	Bad  []BadScript `json:"bad,omitempty" yaml:"bad,omitempty"`
}

// Script is what a scripted model replies, in order, across the whole scenario.
type Script struct {
	Replies []Reply `json:"replies" yaml:"replies"`
	// Extra is for the consumer's scripted Target, such as what a second model
	// replies. The library does not read it.
	Extra map[string]any `json:"extra,omitempty" yaml:"extra,omitempty"`
}

// Reply is one scripted model reply: text, one tool call, or text and a call.
// Args is the call's JSON arguments and may name variables as {{name}}.
type Reply struct {
	Say  string `json:"say,omitempty" yaml:"say,omitempty"`
	Call string `json:"call,omitempty" yaml:"call,omitempty"`
	Args string `json:"args,omitempty" yaml:"args,omitempty"`
}

// BadScript is a script that must fail exactly the check kinds in Fails.
type BadScript struct {
	Script `yaml:",inline"`
	Fails  []string `json:"fails" yaml:"fails"`
}

// Validate refuses a scenario that cannot run or cannot be graded with kinds.
func Validate(s Scenario, kinds Kinds) error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if s.Name == "" {
		bad("no name")
	}
	if len(s.Turns) == 0 {
		bad("no turns")
	}
	if len(s.Checks) == 0 {
		bad("no checks")
	}
	for i, t := range s.Turns {
		if t.Say == "" && t.Before == "" {
			bad("turn %d: say, before or both", i+1)
		}
	}
	for _, c := range s.Checks {
		k, ok := kinds[c.Kind]
		if !ok {
			bad("unknown check %q", c.Kind)
			continue
		}
		if err := k.validate(c); err != nil {
			bad("%w", err)
		}
	}
	scripts := slices.Clone(s.Selftest.Good)
	for i, b := range s.Selftest.Bad {
		if len(b.Fails) == 0 {
			bad("bad script %d names no failing check", i+1)
		}
		for _, kind := range b.Fails {
			if !slices.ContainsFunc(s.Checks, func(c Spec) bool { return c.Kind == kind }) {
				bad("bad script %d fails %q, which the scenario does not check", i+1, kind)
			}
		}
		scripts = append(scripts, b.Script)
	}
	for _, sc := range scripts {
		if len(sc.Replies) == 0 {
			bad("a selftest script has no replies")
		}
		for i, r := range sc.Replies {
			if r.Say == "" && r.Call == "" {
				bad("selftest reply %d: say, call or both", i+1)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("scenario %q: %w", s.Name, err)
	}
	return nil
}
