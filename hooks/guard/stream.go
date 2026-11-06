package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"regexp/syntax"
	"strings"
	"unicode/utf8"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
)

// engine applies rules in order, like Redactor does, and can do it on the part of a
// text that follows an already released prefix. replace turns one match into its
// replacement and may fail, which fails the whole text.
type engine struct {
	rules   []Rule
	replace func(ctx context.Context, r Rule, match string) (string, error)
}

// apply rewrites text. lead is the raw text that precedes it and was released
// already: it is context for assertions such as \b and is never rewritten.
func (e engine) apply(ctx context.Context, lead, text string) (string, error) {
	for _, r := range e.rules {
		full := lead + text
		var b strings.Builder
		b.WriteString(lead)
		last := len(lead)
		for _, m := range r.Pattern.FindAllStringIndex(full, -1) {
			if m[0] < len(lead) {
				continue
			}
			rep, err := e.replace(ctx, r, full[m[0]:m[1]])
			if err != nil {
				return "", err
			}
			b.WriteString(full[last:m[0]])
			b.WriteString(rep)
			last = m[1]
		}
		b.WriteString(full[last:])
		text = b.String()[len(lead):]
	}
	return text, nil
}

// maxLen is the longest match of a pattern in bytes, or -1 if it has none.
func maxLen(r Rule) (int, error) {
	re, err := syntax.Parse(r.Pattern.String(), syntax.Perl)
	if err != nil {
		return 0, err
	}
	n := reMax(re.Simplify())
	if n < 0 {
		return 0, fmt.Errorf("guard: rule %q has no upper bound on a match; bound it with {m,n}", r.Name)
	}
	return n, nil
}

func reMax(re *syntax.Regexp) int {
	switch re.Op {
	case syntax.OpLiteral:
		if re.Flags&syntax.FoldCase != 0 {
			return 4 * len(re.Rune)
		}
		n := 0
		for _, r := range re.Rune {
			n += utf8.RuneLen(r)
		}
		return n
	case syntax.OpCharClass:
		n := 1
		for i := 0; i+1 < len(re.Rune); i += 2 {
			n = max(n, utf8.RuneLen(re.Rune[i+1]))
		}
		return max(n, 4*b2i(re.Flags&syntax.FoldCase != 0))
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return 4
	case syntax.OpStar, syntax.OpPlus:
		if reMax(re.Sub[0]) == 0 {
			return 0
		}
		return -1
	case syntax.OpRepeat:
		if re.Max < 0 {
			return -1
		}
		n := reMax(re.Sub[0])
		if n < 0 {
			return -1
		}
		return n * re.Max
	case syntax.OpQuest, syntax.OpCapture:
		return reMax(re.Sub[0])
	case syntax.OpConcat:
		n := 0
		for _, s := range re.Sub {
			m := reMax(s)
			if m < 0 {
				return -1
			}
			n += m
		}
		return n
	case syntax.OpAlternate:
		n := 0
		for _, s := range re.Sub {
			m := reMax(s)
			if m < 0 {
				return -1
			}
			n = max(n, m)
		}
		return n
	}
	return 0
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// longest is the largest match any rule can make, or -1 if a rule has no bound.
func longest(rules []Rule) (int, error) {
	n := 0
	for _, r := range rules {
		m, err := maxLen(r)
		if err != nil {
			if strings.Contains(err.Error(), "no upper bound") {
				n = -1
				continue
			}
			return 0, err
		}
		if n >= 0 {
			n = max(n, m)
		}
	}
	return n, nil
}

// streamer is the stream half of a rule-based redactor.
type streamer struct {
	engine
	window int // bytes held back; negative holds the whole reply
}

func newStreamer(e engine, window int) (streamer, error) {
	n, err := longest(e.rules)
	if err != nil {
		return streamer{}, err
	}
	if n < 0 {
		return streamer{}, fmt.Errorf("guard: a rule has no upper bound on a match; bound it with {m,n} so a window can hold it")
	}
	if window == 0 {
		window = max(n, 1)
	}
	if window < max(n, 1) {
		return streamer{}, fmt.Errorf("guard: window %d is smaller than the longest match %d", window, n)
	}
	return streamer{e, window}, nil
}

// cut is how much of pending can be released: all but the last window bytes, moved
// forward past any match that straddles that point so no match is split.
func (s streamer) cut(lead, pending string) int {
	if s.window < 0 {
		return 0
	}
	c := len(pending) - s.window
	if c <= 0 {
		return 0
	}
	for c > 0 && !utf8.RuneStart(pending[c]) {
		c--
	}
	full := lead + pending
	for moved := true; moved; {
		moved = false
		for _, r := range s.rules {
			for _, m := range r.Pattern.FindAllStringIndex(full, -1) {
				if s0, e0 := m[0]-len(lead), m[1]-len(lead); s0 < c && c < e0 {
					c, moved = e0, true
				}
			}
		}
	}
	return min(c, len(pending))
}

type heldCall struct {
	id, name string
	args     strings.Builder
}

// WrapModel holds back the tail of the reply text, releases the rest redacted, and
// releases the tail with the final event. Tool-call arguments are collected and
// released redacted before it, so the JSON stays valid.
func (s streamer) WrapModel(_ harness.Run, next model.Provider) model.Provider {
	return model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
		return func(yield func(model.Event, error) bool) {
			var lead, pending string
			var calls []*heldCall
			flush := func() bool {
				out, err := s.apply(ctx, lead, pending)
				if err != nil {
					yield(nil, err)
					return false
				}
				pending = ""
				if out != "" && !yield(model.TextDelta{Text: out}, nil) {
					return false
				}
				for i, c := range calls {
					args, err := s.json(ctx, c.args.String())
					if err != nil {
						yield(nil, err)
						return false
					}
					if !yield(model.ToolCallDelta{Index: i, ID: c.id, Name: c.name, Arguments: args}, nil) {
						return false
					}
				}
				calls = nil
				return true
			}
			released := false
			for ev, err := range next.Complete(ctx, req) {
				if err != nil {
					yield(nil, err)
					return
				}
				switch e := ev.(type) {
				case model.TextDelta:
					pending += e.Text
					c := s.cut(lead, pending)
					if c == 0 {
						continue
					}
					out, err := s.apply(ctx, lead, pending[:c])
					if err != nil {
						yield(nil, err)
						return
					}
					lead = tail(pending[:c])
					pending = pending[c:]
					if out != "" && !yield(model.TextDelta{Text: out}, nil) {
						return
					}
				case model.ToolCallDelta:
					for e.Index >= len(calls) {
						calls = append(calls, &heldCall{})
					}
					c := calls[e.Index]
					c.id, c.name = cmp(e.ID, c.id), cmp(e.Name, c.name)
					c.args.WriteString(e.Arguments)
				case model.Finish:
					if !flush() {
						return
					}
					released = true
					if !yield(e, nil) {
						return
					}
				default:
					if !yield(ev, nil) {
						return
					}
				}
			}
			if !released {
				flush()
			}
		}
	})
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// tail is the last rune of text, the context the next piece needs for \b.
func tail(text string) string {
	if text == "" {
		return ""
	}
	_, n := utf8.DecodeLastRuneInString(text)
	return text[len(text)-n:]
}

// json redacts the strings of a tool call's arguments and leaves valid JSON valid.
// Arguments that are not JSON pass through, for the collector to reject.
func (s streamer) json(ctx context.Context, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" || !json.Valid([]byte(raw)) {
		return raw, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	v, err := mapStrings(v, func(x string) (string, error) { return s.apply(ctx, "", x) })
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

func mapStrings(v any, f func(string) (string, error)) (any, error) {
	switch x := v.(type) {
	case string:
		return f(x)
	case []any:
		for i := range x {
			r, err := mapStrings(x[i], f)
			if err != nil {
				return nil, err
			}
			x[i] = r
		}
	case map[string]any:
		for k, e := range x {
			r, err := mapStrings(e, f)
			if err != nil {
				return nil, err
			}
			x[k] = r
		}
	}
	return v, nil
}

// ruleRedactor is what Stream returns: Redactor over the same rules, and the stream
// filter.
type ruleRedactor struct{ streamer }

func (ruleRedactor) Name() string { return "guard.stream" }

func (r ruleRedactor) Redact(ctx context.Context, _ harness.Kind, text string) (string, error) {
	return r.apply(ctx, "", text)
}

// Stream makes a rule-based redactor live on a stream. It holds back the last
// window bytes of the reply, redacts everything before them as deltas pass, and
// releases the rest with the end of the reply. A match is never split, so no
// delta carries the front half of one. The result is a harness.Redactor and a
// harness.ModelMiddleware, so the ring does not buffer the reply.
//
// A match is replaced by "[redacted:<name>]", as Redactor does. A rule whose
// matches can exceed window, or have no upper bound, is a configuration error
// reported here and not a leak at run time. A window of zero means the longest
// match any rule can make.
func Stream(rules []Rule, window int) (harness.Redactor, error) {
	e := engine{rules: rules, replace: func(_ context.Context, r Rule, _ string) (string, error) {
		return "[redacted:" + r.Name + "]", nil
	}}
	s, err := newStreamer(e, window)
	if err != nil {
		return nil, err
	}
	return ruleRedactor{s}, nil
}
