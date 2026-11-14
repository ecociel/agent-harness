package guard

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Fuzzy is a Finder that matches terms with spelling and OCR errors. Terms and text
// are normalised as a Dictionary does, and the OCR confusions rn and m, ü and ii, 0
// and O, l and 1 are folded together, so they cost nothing. A term of 5 to 8 runes
// in that form may then be one edit (an insertion, a deletion or a substitution)
// away, a longer one two, a shorter one none. It matches
// whole words only, ignores case (CaseSensitive has no effect), and a match never
// spans a run of whitespace over 16 bytes, so MaxLen is bounded. Of several
// matches of one term at one start it keeps the closest. It is safe for concurrent
// use.
type Fuzzy struct {
	ac     automaton
	pieces []piece // by pattern index in ac
	terms  []fuzzyTerm
	maxLen int
}

// piece is one of the k+1 parts a term is cut into. A match with at most k edits
// leaves at least one part intact, so finding the parts exactly finds every
// candidate.
type piece struct {
	term, off, len int
}

type fuzzyTerm struct {
	runes []rune
	k     int
	span  Span
	// Which positions of runes hold each rune, for terms of up to 64 runes: common
	// runes by slot, the rest in peq.
	common [slots]uint64
	peq    []peqEntry
	long   bool
}

// slots are the runes a folded text is mostly made of: a to z, the digits, space.
const slots = 26 + 10 + 1

func slot(r rune) int {
	switch {
	case r >= 'a' && r <= 'z':
		return int(r - 'a')
	case r >= '0' && r <= '9':
		return 26 + int(r-'0')
	case r == ' ':
		return 36
	}
	return -1
}

type peqEntry struct {
	r    rune
	mask uint64
}

// edits is how many edits a term of n runes may be away.
func edits(n int) int {
	switch {
	case n > 8:
		return 2
	case n >= 5:
		return 1
	}
	return 0
}

// NewFuzzy builds a Fuzzy finder. A term with an empty Text, or one shorter than
// MinLen, is skipped. A Class that cannot be part of a token is an error.
func NewFuzzy(terms []Term, o TermOptions) (*Fuzzy, error) {
	z := &Fuzzy{}
	seen := map[string]bool{}
	for i, t := range terms {
		if err := checkClass(t.Class); err != nil {
			return nil, fmt.Errorf("guard: term %d: %w", i, err)
		}
		f := ocr(fold(strings.TrimSpace(t.Text), false)).runes
		if len(f) == 0 || len(f) < o.MinLen || seen[string(f)] {
			continue
		}
		seen[string(f)] = true
		k := edits(len(f))
		z.terms = append(z.terms, newFuzzyTerm(f, k, Span{Class: t.Class, Key: t.Key, Value: t.Value}))
		z.maxLen = max(z.maxLen, termBytes(f, k))
	}
	grams := map[uint64]int32{}
	for _, t := range z.terms {
		for q := 1; q <= 3; q++ {
			for i := 0; i+q <= len(t.runes); i++ {
				grams[gram(t.runes[i:i+q])]++
			}
		}
	}
	var patterns [][]rune
	for n, t := range z.terms {
		size := len(t.runes) / (t.k + 1)
		for _, off := range rarest(t.runes, t.k+1, size, grams) {
			patterns = append(patterns, t.runes[off:off+size])
			z.pieces = append(z.pieces, piece{n, off, size})
		}
	}
	z.ac = newAutomaton(patterns)
	return z, nil
}

func gram(rs []rune) uint64 {
	g := uint64(len(rs))
	for _, r := range rs {
		g = g<<21 | uint64(r)
	}
	return g
}

// rarest places count disjoint pieces of size runes in term where the terms share
// the fewest of their q-grams, and returns their offsets. A hit on a piece is
// checked against every term that has it, so a piece made of a common first name
// would send each such name in a text to thousands of terms. How many terms share
// a piece is at most how many share its rarest q-gram.
func rarest(term []rune, count, size int, grams map[uint64]int32) []int {
	q := min(3, size)
	cost := func(off int) int64 {
		c := int64(1 << 40)
		for i := off; i+q <= off+size; i++ {
			c = min(c, int64(grams[gram(term[i:i+q])]))
		}
		return c
	}
	n := len(term)
	const none = int64(1 << 62)
	// best[j][i] is the least cost of j pieces within term[:i].
	best := make([][]int64, count+1)
	for j := range best {
		best[j] = make([]int64, n+1)
		for i := range best[j] {
			if j > 0 {
				best[j][i] = none
			}
		}
	}
	for j := 1; j <= count; j++ {
		for i := 1; i <= n; i++ {
			best[j][i] = best[j][i-1]
			if i >= size && best[j-1][i-size] < none {
				best[j][i] = min(best[j][i], best[j-1][i-size]+cost(i-size))
			}
		}
	}
	offs := make([]int, count)
	for j, i := count, n; j > 0; {
		if best[j][i] == best[j][i-1] {
			i--
			continue
		}
		offs[j-1] = i - size
		i -= size
		j--
	}
	return offs
}

// ocr writes the confusions of an OCR reading in one form: rn as m, ii as ue (the
// folded ü), 0 as o and 1 as l.
func ocr(f folded) folded {
	out := folded{
		runes: make([]rune, 0, len(f.runes)),
		start: make([]int, 0, len(f.runes)),
		end:   make([]int, 0, len(f.runes)),
	}
	emit := func(r rune, s, e int) {
		out.runes = append(out.runes, r)
		out.start = append(out.start, s)
		out.end = append(out.end, e)
	}
	for i := 0; i < len(f.runes); i++ {
		r := f.runes[i]
		if i+1 < len(f.runes) {
			switch next := f.runes[i+1]; {
			case r == 'r' && next == 'n':
				emit('m', f.start[i], f.end[i+1])
				i++
				continue
			case r == 'i' && next == 'i':
				emit('u', f.start[i], f.end[i+1])
				emit('e', f.start[i], f.end[i+1])
				i++
				continue
			}
		}
		switch r {
		case '0':
			r = 'o'
		case '1':
			r = 'l'
		}
		emit(r, f.start[i], f.end[i])
	}
	return out
}

// Find implements Finder.
func (z *Fuzzy) Find(_ context.Context, text string) ([]Span, error) {
	if len(z.terms) == 0 {
		return nil, nil
	}
	f := ocr(fold(text, false))
	runes := f.runes
	word := make([]bool, len(runes)+1)
	for i, r := range runes {
		word[i] = isWord(r)
	}
	var out []Span
	var dist []int
	z.ac.each(runes, func(p int32, end int) {
		pc := z.pieces[p]
		t := &z.terms[pc.term]
		n := len(t.runes)
		aligned := end - pc.len - pc.off
		for s := max(aligned-t.k, 0); s <= aligned+t.k && s < len(runes); s++ {
			if !word[s] || s > 0 && word[s-1] {
				continue
			}
			cand := runes[s:min(s+n+t.k, len(runes))]
			dist = t.distances(cand, dist)
			best, bestEnd := t.k+1, 0
			for e := max(s+n-t.k, s+1); e <= s+len(cand); e++ {
				d := dist[e-s]
				if d > t.k || !word[e-1] || word[e] || hasGap(runes[s:e]) {
					continue
				}
				if d < best || d == best && abs(e-s-n) < abs(bestEnd-s-n) {
					best, bestEnd = d, e
				}
			}
			if best <= t.k {
				sp := t.span
				sp.Start, sp.End = f.start[s], f.end[bestEnd-1]
				out = append(out, sp)
			}
		}
	})
	// A match that keeps several of its pieces is found once for each.
	slices.SortFunc(out, func(a, b Span) int {
		return cmpSpan(a, b)
	})
	return slices.CompactFunc(out, func(a, b Span) bool { return cmpSpan(a, b) == 0 }), nil
}

func cmpSpan(a, b Span) int {
	if a.Start != b.Start {
		return a.Start - b.Start
	}
	if a.End != b.End {
		return a.End - b.End
	}
	for _, c := range []int{strings.Compare(a.Class, b.Class), strings.Compare(a.Key, b.Key), strings.Compare(a.Value, b.Value)} {
		if c != 0 {
			return c
		}
	}
	return 0
}

// MaxLen implements Finder.
func (z *Fuzzy) MaxLen() int { return z.maxLen }

func hasGap(rs []rune) bool {
	for _, r := range rs {
		if r == gap {
			return true
		}
	}
	return false
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func newFuzzyTerm(term []rune, k int, span Span) fuzzyTerm {
	t := fuzzyTerm{runes: term, k: k, span: span, long: len(term) > 64}
	if t.long {
		return t
	}
next:
	for i, r := range term {
		if c := slot(r); c >= 0 {
			t.common[c] |= 1 << i
			continue
		}
		for j := range t.peq {
			if t.peq[j].r == r {
				t.peq[j].mask |= 1 << i
				continue next
			}
		}
		t.peq = append(t.peq, peqEntry{r, 1 << i})
	}
	return t
}

func (t *fuzzyTerm) eq(r rune) uint64 {
	if c := slot(r); c >= 0 {
		return t.common[c]
	}
	for _, p := range t.peq {
		if p.r == r {
			return p.mask
		}
	}
	return 0
}

// distances sets dst[j] to the edit distance between the term and cand[:j], for
// every j up to len(cand), and returns dst. Terms of up to 64 runes use the
// bit-parallel algorithm of Myers in Hyyrö's form for a global distance, one pass
// for all ends; longer ones a plain table.
func (t *fuzzyTerm) distances(cand []rune, dst []int) []int {
	dst = append(dst[:0], len(t.runes))
	if t.long {
		for j := 1; j <= len(cand); j++ {
			dst = append(dst, distance(t.runes, cand[:j]))
		}
		return dst
	}
	m := len(t.runes)
	last := uint64(1) << (m - 1)
	pv, mv, score := ^uint64(0), uint64(0), m
	for _, c := range cand {
		eq := t.eq(c)
		xv := eq | mv
		xh := ((eq & pv) + pv) ^ pv | eq
		ph := mv | ^(xh | pv)
		mh := pv & xh
		if ph&last != 0 {
			score++
		} else if mh&last != 0 {
			score--
		}
		ph = ph<<1 | 1
		mh <<= 1
		pv = mh | ^(xv | ph)
		mv = ph & xv
		dst = append(dst, score)
	}
	return dst
}

// distance is the edit distance of a and b.
func distance(a, b []rune) int {
	row := make([]int, len(b)+1)
	for j := range row {
		row[j] = j
	}
	for i := 1; i <= len(a); i++ {
		diag := row[0]
		row[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			diag, row[j] = row[j], min(row[j]+1, row[j-1]+1, diag+cost)
		}
	}
	return row[len(b)]
}
