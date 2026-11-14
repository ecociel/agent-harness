package guard

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Term is one entry of a Dictionary or Fuzzy finder.
type Term struct {
	Text  string // what is looked for, e.g. "Anna Müller"
	Class string // goes into the token, e.g. "person"
	Key   string // what the token is keyed on, e.g. "party/4711"; empty means the matched text
	Value string // what reveal returns; empty means the matched text
	// CaseSensitive matches the term in its own case only, as for an
	// abbreviation that is also a common word.
	CaseSensitive bool
}

// TermOptions configure a Dictionary or Fuzzy finder.
type TermOptions struct {
	// MinLen drops terms shorter than this many runes once normalised, so a
	// two-letter name does not match every word it spells.
	MinLen int
}

// Dictionary is a Finder for a fixed set of terms. It matches whole words only,
// ignores case (unless a term is CaseSensitive), takes ü and ue, ä and ae, ö and oe,
// ß and ss for the same, and takes any run of whitespace up to 16 bytes for one
// space. Spans cover the original bytes. A term that normalises to the same text as
// an earlier one is dropped. It is safe for concurrent use.
type Dictionary struct {
	ac     automaton
	terms  []dictTerm // by pattern index in ac
	maxLen int
}

type dictTerm struct {
	n     int    // runes in the folded form
	exact []rune // the case-sensitive folded form, nil when the term ignores case
	span  Span
}

// NewDictionary builds a Dictionary. A term with an empty Text, or one shorter than
// MinLen, is skipped. A Class that cannot be part of a token is an error.
func NewDictionary(terms []Term, o TermOptions) (*Dictionary, error) {
	d := &Dictionary{}
	var patterns [][]rune
	seen := map[string]bool{}
	for i, t := range terms {
		if err := checkClass(t.Class); err != nil {
			return nil, fmt.Errorf("guard: term %d: %w", i, err)
		}
		f := foldTerm(t.Text, false)
		if len(f) == 0 || len(f) < o.MinLen {
			continue
		}
		var exact []rune
		id := "i" + string(f)
		if t.CaseSensitive {
			exact = foldTerm(t.Text, true)
			id = "s" + string(exact)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		patterns = append(patterns, f)
		d.terms = append(d.terms, dictTerm{n: len(f), exact: exact, span: Span{Class: t.Class, Key: t.Key, Value: t.Value}})
		d.maxLen = max(d.maxLen, termBytes(f, 0))
	}
	d.ac = newAutomaton(patterns)
	return d, nil
}

// foldTerm is the folded runes of a term's text, trimmed.
func foldTerm(text string, caseSensitive bool) []rune {
	return fold(strings.TrimSpace(text), caseSensitive).runes
}

// termBytes bounds how many original bytes can fold into f with extra runes
// inserted: a rune comes from at most four bytes, a space from a run of up to
// maxSpace, and an inserted rune may be either.
func termBytes(f []rune, extra int) int {
	spaces := 0
	for _, r := range f {
		if r == ' ' {
			spaces++
		}
	}
	return 4*(len(f)-spaces) + maxSpace*(spaces+extra)
}

// Find implements Finder.
func (d *Dictionary) Find(_ context.Context, text string) ([]Span, error) {
	if len(d.terms) == 0 {
		return nil, nil
	}
	f := fold(text, false)
	var out []Span
	d.ac.each(f.runes, func(p int32, end int) {
		t := &d.terms[p]
		start := end - t.n
		if !whole(f.runes, start, end) {
			return
		}
		s := t.span
		s.Start, s.End = f.start[start], f.end[end-1]
		if t.exact != nil && !slices.Equal(fold(text[s.Start:s.End], true).runes, t.exact) {
			return
		}
		out = append(out, s)
	})
	return out, nil
}

// MaxLen implements Finder.
func (d *Dictionary) MaxLen() int { return d.maxLen }
