package guard_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/guard"
)

// literals finds each of its texts wherever it occurs, with the class, key and
// value given for it.
type literals map[string]guard.Span

func (l literals) Find(_ context.Context, text string) ([]guard.Span, error) {
	var out []guard.Span
	for lit, s := range l {
		for i := 0; ; {
			j := strings.Index(text[i:], lit)
			if j < 0 {
				break
			}
			s.Start, s.End = i+j, i+j+len(lit)
			out = append(out, s)
			i += j + 1
		}
	}
	return out, nil
}

func (l literals) MaxLen() int {
	n := 0
	for lit := range l {
		n = max(n, len(lit))
	}
	return n
}

type finderFunc struct {
	find func(text string) ([]guard.Span, error)
	max  int
}

func (f finderFunc) Find(_ context.Context, text string) ([]guard.Span, error) { return f.find(text) }
func (f finderFunc) MaxLen() int                                               { return f.max }

var keller = literals{
	"Keller AG": {Class: "org", Key: "party/1", Value: "Keller AG"},
	"KELLER AG": {Class: "org", Key: "party/1", Value: "Keller AG"},
}

func TestTwoSpellingsOfOneKeyShareATokenThatRevealsTheValue(t *testing.T) {
	ctx := context.Background()
	v := guard.NewMemVault()
	p := guard.Pseudonymizer{Finders: []guard.Finder{keller}, Key: []byte("k"), Vault: v}
	out, err := p.Redact(ctx, harness.KindToolDone, "Keller AG wrote, KELLER AG signed.")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, ok := strings.Cut(out, " wrote, ")
	if !ok || !strings.HasPrefix(tok, "⟦org:") || out != tok+" wrote, "+tok+" signed." {
		t.Fatalf("%q", out)
	}
	if got, _ := guard.RevealText(ctx, v, out); got != "Keller AG wrote, Keller AG signed." {
		t.Fatalf("revealed %q", got)
	}
}

func TestASpanWithoutKeyOrValueUsesTheMatchedText(t *testing.T) {
	ctx := context.Background()
	v := guard.NewMemVault()
	words := literals{"Anna": {Class: "person"}}
	rule := guard.Pseudonymizer{Rules: []guard.Rule{guard.R("person", `Anna`)}, Key: []byte("k"), Vault: v}
	find := guard.Pseudonymizer{Finders: []guard.Finder{words}, Key: []byte("k"), Vault: v}
	a, _ := rule.Redact(ctx, harness.KindToolDone, "hi Anna")
	b, _ := find.Redact(ctx, harness.KindToolDone, "hi Anna")
	if a != b || a == "hi Anna" {
		t.Fatalf("rule %q finder %q", a, b)
	}
	if got, _ := guard.RevealText(ctx, v, b); got != "hi Anna" {
		t.Fatal(got)
	}
}

func TestOverlapsBetweenRulesAndFinders(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		p     guard.Pseudonymizer
		class string
	}{
		"a longer finder span beats a rule": {guard.Pseudonymizer{
			Rules:   []guard.Rule{guard.R("word", `Keller`)},
			Finders: []guard.Finder{keller}}, "org"},
		"on a tie the rule wins": {guard.Pseudonymizer{
			Rules:   []guard.Rule{guard.R("rule", `Keller AG`)},
			Finders: []guard.Finder{keller}}, "rule"},
		"on a tie the earlier finder wins": {guard.Pseudonymizer{
			Finders: []guard.Finder{literals{"Keller AG": {Class: "first"}}, keller}}, "first"},
	} {
		c.p.Key, c.p.Vault = []byte("k"), guard.NewMemVault()
		out, err := c.p.Redact(ctx, harness.KindToolDone, "to Keller AG.")
		if err != nil || !strings.HasPrefix(out, "to ⟦"+c.class+":") || strings.Count(out, "⟦") != 1 || !strings.HasSuffix(out, "⟧.") {
			t.Errorf("%s: %v %q", name, err, out)
		}
	}
}

func TestAFinderFailureFailsClosed(t *testing.T) {
	ctx := context.Background()
	for name, f := range map[string]finderFunc{
		"error": {find: func(string) ([]guard.Span, error) { return nil, errors.New("down") }, max: 8},
		"out of range": {find: func(s string) ([]guard.Span, error) {
			return []guard.Span{{Start: 0, End: len(s) + 1, Class: "x"}}, nil
		}, max: 8},
		"inside a rune": {find: func(s string) ([]guard.Span, error) {
			return []guard.Span{{Start: strings.Index(s, "ü") + 1, End: len(s), Class: "x"}}, nil
		}, max: 8},
	} {
		p := guard.Pseudonymizer{Finders: []guard.Finder{f}, Key: []byte("k"), Vault: guard.NewMemVault()}
		if out, err := p.Redact(ctx, harness.KindToolDone, "Müller"); err == nil {
			t.Errorf("%s: %q", name, out)
		}
	}
}

func TestAFinderStreamsWithinItsBound(t *testing.T) {
	p := guard.Pseudonymizer{Finders: []guard.Finder{keller}, Key: []byte("k"), Vault: guard.NewMemVault()}
	text := strings.Repeat("filler ", 6) + "Keller AG and KELLER AG, " + strings.Repeat("more text ", 5) + "Keller AG"
	want, err := p.Redact(context.Background(), harness.KindDelta, text)
	if err != nil || strings.Contains(want, "eller") {
		t.Fatalf("%v %q", err, want)
	}
	for n := 1; n <= 30; n++ {
		if streamed, out := deltas(t, chunked(text, n), p); streamed != want || out != want {
			t.Fatalf("chunk %d: %q", n, streamed)
		}
	}
}

func TestAnUnboundedFinderHoldsTheWholeReply(t *testing.T) {
	unbounded := finderFunc{find: func(s string) ([]guard.Span, error) { return keller.Find(context.Background(), s) }, max: -1}
	p := guard.Pseudonymizer{Finders: []guard.Finder{unbounded}, Key: []byte("k"), Vault: guard.NewMemVault()}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	streamed, out := deltas(t, chunked(strings.Repeat("x ", 100)+"Keller AG", 3), p)
	if streamed != out || strings.Contains(out, "Keller") {
		t.Fatalf("%q", out)
	}
	p.Window = 64
	if err := p.Validate(); err == nil {
		t.Fatal("a window was accepted for an unbounded finder")
	}
}
