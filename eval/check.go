package eval

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Spec is one check in a scenario: a kind and its arguments. Which of Args and N a
// kind reads is its Shape.
type Spec struct {
	Kind string   `json:"kind" yaml:"kind"`
	Args []string `json:"args,omitempty" yaml:"args,omitempty"`
	N    int      `json:"n,omitempty" yaml:"n,omitempty"`
}

// Shape says which arguments a kind takes, so a consumer can decode a scenario
// file and Validate can refuse a malformed check before anything runs.
type Shape int

const (
	List         Shape = iota // a non-empty Args
	Patterns                  // a non-empty Args of regular expressions
	Count                     // N
	ListAndCount              // a non-empty Args and N
	Flag                      // nothing: the kind is on or absent
)

// Kind is one check kind. Check returns why the observation fails the spec, or
// the empty string when it passes.
type Kind struct {
	Shape Shape
	Check func(o Observation, s Spec) (why string)
}

// Kinds is a registry of check kinds by name. A consumer starts from Generic and
// adds its own.
type Kinds map[string]Kind

// Failure is one failed check.
type Failure struct {
	Kind string `json:"kind"`
	Why  string `json:"why"`
}

func (f Failure) String() string { return f.Kind + ": " + f.Why }

// Grade runs every check of s on o and returns the failures in check order.
func Grade(s Scenario, o Observation, kinds Kinds) []Failure {
	var out []Failure
	for _, c := range s.Checks {
		k, ok := kinds[c.Kind]
		if !ok {
			out = append(out, Failure{c.Kind, "unknown check kind"})
			continue
		}
		if why := k.Check(o, c); why != "" {
			out = append(out, Failure{c.Kind, why})
		}
	}
	return out
}

func (k Kind) validate(s Spec) error {
	switch k.Shape {
	case List, Patterns, ListAndCount:
		if len(s.Args) == 0 {
			return fmt.Errorf("%s takes a non-empty list", s.Kind)
		}
	}
	if k.Shape == Patterns {
		for _, p := range s.Args {
			if _, err := regexp.Compile(p); err != nil {
				return fmt.Errorf("%s: %w", s.Kind, err)
			}
		}
	}
	return nil
}

// Generic returns the kinds every consumer gets. The answer kinds read the last
// turn's answer, except answer_lacks and answer_lacks_match, which read every
// answer. The call kinds count every call the model asked for in any turn, whether
// it ran, waits or was denied. Text matching goes through Mentions.
//
//	answer_has          List          the last answer mentions every item
//	answer_any          List          the last answer mentions at least one item
//	answer_matches      Patterns      the last answer matches every pattern
//	answer_lacks        List          no answer mentions any item
//	answer_lacks_match  Patterns      no answer matches any pattern
//	called              List          every tool was called
//	called_any          List          at least one tool was called
//	not_called          List          no tool was called
//	tools_within        List          every call is to one of the tools
//	at_most_once        List          no tool was called twice
//	calls_at_most       ListAndCount  the tools were called at most N times together
//	waited_for          List          a call to every tool opened a wait
//	no_wait             Flag          no call opened a wait
//	final_stop          List          the last turn stopped for one of the reasons
//	not_echoed          List          no call's arguments and no Facts[Surfaces] text mention an item
func Generic() Kinds {
	return Kinds{
		"answer_has": {List, func(o Observation, s Spec) string {
			return listed("the last answer lacks", absent(o.Answer(), s.Args))
		}},
		"answer_any": {List, func(o Observation, s Spec) string {
			if len(absent(o.Answer(), s.Args)) < len(s.Args) {
				return ""
			}
			return "the last answer has none of " + quoted(s.Args)
		}},
		"answer_matches": {Patterns, func(o Observation, s Spec) string {
			var missing []string
			for _, p := range s.Args {
				if re, err := regexp.Compile(p); err != nil || !re.MatchString(o.Answer()) {
					missing = append(missing, p)
				}
			}
			return listed("the last answer does not match", missing)
		}},
		"answer_lacks": {List, func(o Observation, s Spec) string {
			all := strings.Join(o.Answers(), "\n")
			var found []string
			for _, a := range s.Args {
				if Mentions(all, a) {
					found = append(found, a)
				}
			}
			return listed("an answer contains", found)
		}},
		"answer_lacks_match": {Patterns, func(o Observation, s Spec) string {
			all := strings.Join(o.Answers(), "\n")
			var found []string
			for _, p := range s.Args {
				re, err := regexp.Compile(p)
				if err != nil {
					return "bad pattern " + strconv.Quote(p)
				}
				if m := re.FindString(all); m != "" {
					found = append(found, m)
				}
			}
			return listed("an answer contains", found)
		}},
		"called": {List, func(o Observation, s Spec) string {
			calls := o.Calls()
			var missing []string
			for _, name := range s.Args {
				if !slices.ContainsFunc(calls, func(c Call) bool { return c.Tool == name }) {
					missing = append(missing, name)
				}
			}
			return listed("never called", missing)
		}},
		"called_any": {List, func(o Observation, s Spec) string {
			if slices.ContainsFunc(o.Calls(), func(c Call) bool { return slices.Contains(s.Args, c.Tool) }) {
				return ""
			}
			return "called none of " + quoted(s.Args)
		}},
		"not_called": {List, func(o Observation, s Spec) string {
			return listed("called", tools(o.Calls(), func(c Call) bool { return slices.Contains(s.Args, c.Tool) }))
		}},
		"tools_within": {List, func(o Observation, s Spec) string {
			return listed("called outside the allowed set", tools(o.Calls(), func(c Call) bool { return !slices.Contains(s.Args, c.Tool) }))
		}},
		"at_most_once": {List, func(o Observation, s Spec) string {
			calls := o.Calls()
			var repeated []string
			for _, name := range s.Args {
				if n := count(calls, name); n > 1 {
					repeated = append(repeated, fmt.Sprintf("%s (%d times)", name, n))
				}
			}
			return listed("called more than once:", repeated)
		}},
		"calls_at_most": {ListAndCount, func(o Observation, s Spec) string {
			n := 0
			for _, name := range s.Args {
				n += count(o.Calls(), name)
			}
			if n > s.N {
				return fmt.Sprintf("called %s %d times, want at most %d", quoted(s.Args), n, s.N)
			}
			return ""
		}},
		"waited_for": {List, func(o Observation, s Spec) string {
			waits := o.Waits()
			var missing []string
			for _, name := range s.Args {
				if !slices.ContainsFunc(waits, func(w Wait) bool { return w.Tool == name }) {
					missing = append(missing, name)
				}
			}
			return listed("no wait for", missing)
		}},
		"no_wait": {Flag, func(o Observation, _ Spec) string {
			var waited []string
			for _, w := range o.Waits() {
				if w.Tool != "" && !slices.Contains(waited, w.Tool) {
					waited = append(waited, w.Tool)
				}
			}
			return listed("waits opened for", waited)
		}},
		"final_stop": {List, func(o Observation, s Spec) string {
			if len(o.Turns) == 0 {
				return "no turn ran"
			}
			last := string(o.Turns[len(o.Turns)-1].Stop)
			if slices.Contains(s.Args, last) {
				return ""
			}
			return fmt.Sprintf("the last turn stopped %s, want %s", last, strings.Join(s.Args, " or "))
		}},
		"not_echoed": {List, func(o Observation, s Spec) string {
			var found []string
			for _, surface := range surfaces(o) {
				for _, a := range s.Args {
					if Mentions(surface, a) && !slices.Contains(found, a) {
						found = append(found, a)
					}
				}
			}
			return listed("an acted-on surface carries", found)
		}},
	}
}

func surfaces(o Observation) []string {
	var out []string
	for _, c := range o.Calls() {
		out = append(out, string(c.Args))
	}
	if more, ok := o.Facts[Surfaces].([]string); ok {
		out = append(out, more...)
	}
	return out
}

func tools(calls []Call, keep func(Call) bool) []string {
	var out []string
	for _, c := range calls {
		if keep(c) && !slices.Contains(out, c.Tool) {
			out = append(out, c.Tool)
		}
	}
	return out
}

func count(calls []Call, name string) int {
	n := 0
	for _, c := range calls {
		if c.Tool == name {
			n++
		}
	}
	return n
}

// Unseen returns the items the answer_has and answer_any checks of s expect that
// seen, the text the model was given, does not mention. A fact no tool result or
// attachment showed cannot fairly be asked of the model. For answer_any the list
// counts only when none of it was seen.
func Unseen(s Scenario, seen string) []string {
	var out []string
	for _, c := range s.Checks {
		missing := absent(seen, c.Args)
		switch {
		case c.Kind == "answer_has":
			out = append(out, missing...)
		case c.Kind == "answer_any" && len(missing) == len(c.Args):
			out = append(out, c.Args...)
		}
	}
	return out
}

// Mentions reports whether text contains s after both are folded by Canonical.
func Mentions(text, s string) bool {
	return strings.Contains(Canonical(text), Canonical(s))
}

var thousands = regexp.MustCompile(`(\d)[ ',\x{2019}\x{00a0}\x{202f}](\d{3})`)

// Canonical folds case, collapses runs of white space to one space and drops
// thousands separators (space, apostrophe, comma, right single quote, no-break and
// narrow no-break space), so "CHF 1'250'000" and "chf 1,250,000" compare equal.
func Canonical(s string) string {
	s = strings.Join(strings.Fields(strings.ToLower(s)), " ")
	for {
		next := thousands.ReplaceAllString(s, "$1$2")
		if next == s {
			return s
		}
		s = next
	}
}

func absent(text string, want []string) []string {
	var missing []string
	for _, s := range want {
		if !Mentions(text, s) {
			missing = append(missing, s)
		}
	}
	return missing
}

func listed(prefix string, items []string) string {
	if len(items) == 0 {
		return ""
	}
	return prefix + " " + quoted(items)
}

func quoted(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = strconv.Quote(s)
	}
	return strings.Join(q, ", ")
}
