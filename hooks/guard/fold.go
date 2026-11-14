package guard

import (
	"slices"
	"unicode"
	"unicode/utf8"
)

// folded is a text in the form terms are matched in, and where each of its runes
// came from: rune i was made from the original bytes start[i]:end[i].
type folded struct {
	runes      []rune
	start, end []int
}

// gap stands for a run of whitespace too long to fold into one space. No term
// holds it, so no match spans it, which keeps a match's length bounded.
const gap = rune(0)

// maxSpace is the longest run of whitespace, in bytes, that folds into one space.
const maxSpace = 16

// fold lowers the case of text unless caseSensitive, writes umlauts and ß out (ü
// as ue, ß as ss, also u followed by a combining diaeresis), and folds each run of
// whitespace into one space.
func fold(text string, caseSensitive bool) folded {
	f := folded{
		runes: make([]rune, 0, len(text)),
		start: make([]int, 0, len(text)),
		end:   make([]int, 0, len(text)),
	}
	emit := func(r rune, s, e int) {
		f.runes = append(f.runes, r)
		f.start = append(f.start, s)
		f.end = append(f.end, e)
	}
	for i := 0; i < len(text); {
		r, n := utf8.DecodeRuneInString(text[i:])
		if unicode.IsSpace(r) {
			j := i + n
			for j < len(text) {
				r2, n2 := utf8.DecodeRuneInString(text[j:])
				if !unicode.IsSpace(r2) {
					break
				}
				j += n2
			}
			if j-i <= maxSpace {
				emit(' ', i, j)
			} else {
				emit(gap, i, j)
			}
			i = j
			continue
		}
		if !caseSensitive {
			r = unicode.ToLower(r)
		}
		switch r {
		case 'ä', 'ö', 'ü', 'Ä', 'Ö', 'Ü':
			emit(umlautBase(r), i, i+n)
			emit(umlautE(r), i, i+n)
		case 'ß':
			emit('s', i, i+n)
			emit('s', i, i+n)
		case 'ẞ':
			emit('S', i, i+n)
			emit('S', i, i+n)
		case '̈':
			if k := len(f.runes) - 1; k >= 0 && slices.Contains([]rune("aouAOU"), f.runes[k]) && f.end[k] == i {
				emit(umlautE(f.runes[k]), i, i+n)
			} else {
				emit(r, i, i+n)
			}
		default:
			emit(r, i, i+n)
		}
		i += n
	}
	return f
}

func umlautBase(r rune) rune {
	switch r {
	case 'ä':
		return 'a'
	case 'ö':
		return 'o'
	case 'ü':
		return 'u'
	case 'Ä':
		return 'A'
	case 'Ö':
		return 'O'
	}
	return 'U'
}

// umlautE is the e that follows a written-out umlaut, in the case of its vowel.
func umlautE(r rune) rune {
	if unicode.IsUpper(r) {
		return 'E'
	}
	return 'e'
}

// isWord reports whether r is part of a word, so a term next to it is not whole.
func isWord(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

// whole reports whether runes[i:j] stands as whole words.
func whole(runes []rune, i, j int) bool {
	return (i == 0 || !isWord(runes[i-1])) && (j == len(runes) || !isWord(runes[j]))
}

// automaton is an Aho-Corasick automaton over runes. Node 0 is the root.
type automaton struct {
	nodes []acNode
	root  [128]int32 // the root's edges for ASCII, the common case
}

type acNode struct {
	edges []acEdge // sorted by rune
	fail  int32
	out   []int32 // the patterns that end here, own and inherited through fail
}

type acEdge struct {
	r  rune
	to int32
}

func (n *acNode) step(r rune) int32 {
	if len(n.edges) <= 4 {
		for _, e := range n.edges {
			if e.r == r {
				return e.to
			}
		}
		return -1
	}
	i, ok := slices.BinarySearchFunc(n.edges, r, func(e acEdge, r rune) int { return int(e.r - r) })
	if !ok {
		return -1
	}
	return n.edges[i].to
}

// newAutomaton builds the automaton of patterns; a match reports the index of
// its pattern.
func newAutomaton(patterns [][]rune) automaton {
	a := automaton{nodes: []acNode{{}}}
	for p, pat := range patterns {
		s := int32(0)
		for _, r := range pat {
			next := a.nodes[s].step(r)
			if next < 0 {
				next = int32(len(a.nodes))
				a.nodes = append(a.nodes, acNode{})
				n := &a.nodes[s]
				i, _ := slices.BinarySearchFunc(n.edges, r, func(e acEdge, r rune) int { return int(e.r - r) })
				n.edges = slices.Insert(n.edges, i, acEdge{r, next})
			}
			s = next
		}
		a.nodes[s].out = append(a.nodes[s].out, int32(p))
	}
	queue := make([]int32, 0, len(a.nodes))
	for _, e := range a.nodes[0].edges {
		queue = append(queue, e.to)
	}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		for _, e := range a.nodes[s].edges {
			f := a.nodes[s].fail
			for f != 0 && a.nodes[f].step(e.r) < 0 {
				f = a.nodes[f].fail
			}
			if t := a.nodes[f].step(e.r); t >= 0 && t != e.to {
				a.nodes[e.to].fail = t
			}
			if inherited := a.nodes[a.nodes[e.to].fail].out; len(inherited) > 0 {
				a.nodes[e.to].out = append(slices.Clip(a.nodes[e.to].out), inherited...)
			}
			queue = append(queue, e.to)
		}
	}
	for i := range a.root {
		a.root[i] = max(a.nodes[0].step(rune(i)), 0)
	}
	return a
}

func (a *automaton) next(s int32, r rune) int32 {
	for {
		if s == 0 {
			if r < 128 {
				return a.root[r]
			}
			return max(a.nodes[0].step(r), 0)
		}
		if t := a.nodes[s].step(r); t >= 0 {
			return t
		}
		s = a.nodes[s].fail
	}
}

// each calls fn(pattern, end) for every occurrence of every pattern in runes,
// where end is the index after its last rune.
func (a *automaton) each(runes []rune, fn func(pattern int32, end int)) {
	s := int32(0)
	for i, r := range runes {
		s = a.next(s, r)
		for _, p := range a.nodes[s].out {
			fn(p, i+1)
		}
	}
}
