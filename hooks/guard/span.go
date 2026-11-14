package guard

import (
	"context"
	"slices"
	"strings"
	"unicode/utf8"
)

// Span is one piece of a text a Finder wants replaced by a token.
type Span struct {
	Start, End int    // byte offsets in the text given to Find
	Class      string // goes into the token, e.g. "person"
	Key        string // what the token is keyed on, e.g. "party/4711"; empty means the matched text
	Value      string // what reveal returns; empty means the matched text
}

// Finder finds spans in a text, as a Rule does with a pattern. It must be safe
// for concurrent use. Spans may overlap; the Pseudonymizer settles overlaps.
type Finder interface {
	Find(ctx context.Context, text string) ([]Span, error)
	// MaxLen is the longest span in bytes, which bounds a stream's window; -1
	// means no bound, and the stream holds the whole reply.
	MaxLen() int
}

// hit is one candidate span and the source that found it: the index of a rule,
// or of a finder after the rules. A lower source wins a tie.
type hit struct {
	Span
	src int
}

// resolve settles which hits are replaced, and is the only place overlap policy
// lives. A hit that touches a token already in the text is dropped, so redacting
// redacted text changes nothing. Of overlapping hits the longest wins, then the one
// from the earlier source, then the one that starts first; a hit that overlaps a
// winner is dropped whole. The winners come out ordered by start.
func resolve(hits []hit, tokens [][]int) []hit {
	cands := make([]hit, 0, len(hits))
	for _, h := range hits {
		if h.End > h.Start && !touchesToken(h, tokens) {
			cands = append(cands, h)
		}
	}
	slices.SortStableFunc(cands, func(a, b hit) int {
		if d := (b.End - b.Start) - (a.End - a.Start); d != 0 {
			return d
		}
		if a.src != b.src {
			return a.src - b.src
		}
		return a.Start - b.Start
	})
	var won []hit
	for _, h := range cands {
		i, _ := slices.BinarySearchFunc(won, h.Start, func(w hit, start int) int { return w.Start - start })
		if i > 0 && won[i-1].End > h.Start || i < len(won) && won[i].Start < h.End {
			continue
		}
		won = slices.Insert(won, i, h)
	}
	return won
}

func touchesToken(h hit, tokens [][]int) bool {
	i, _ := slices.BinarySearchFunc(tokens, h.Start, func(t []int, start int) int { return t[1] - start - 1 })
	return i < len(tokens) && tokens[i][0] < h.End
}

// tokenRanges are the byte ranges of the tokens in s, in order.
func tokenRanges(s string) [][]int {
	if !strings.Contains(s, "⟦") {
		return nil
	}
	return tokenRe.FindAllStringIndex(s, -1)
}

// partialToken is where a token that has not ended yet starts at the end of s, or
// len(s) if s does not end in one. A stream holds that tail back until it knows
// whether it is a token.
func partialToken(s string) int {
	if i := strings.LastIndex(s, "⟦"); i >= 0 && tokenPrefix(s[i+len("⟦"):]) {
		return i
	}
	for n := 1; n < len("⟦") && n <= len(s); n++ {
		if s[len(s)-n:] == "⟦"[:n] {
			return len(s) - n
		}
	}
	return len(s)
}

// tokenPrefix reports whether "⟦"+rest is the front of a token, not all of one, as
// tokenRe defines it.
func tokenPrefix(rest string) bool {
	class := 0
	for rest != "" && rest[0] != ':' {
		if !utf8.FullRuneInString(rest) {
			return true
		}
		r, n := utf8.DecodeRuneInString(rest)
		if r == '⟦' || r == '⟧' || strings.ContainsRune(" \t\n\f\r", r) {
			return false
		}
		if class++; class > 64 {
			return false
		}
		rest = rest[n:]
	}
	if rest == "" {
		return true
	}
	if class == 0 {
		return false
	}
	rest = rest[1:]
	hex := 0
	for hex < len(rest) && hex < 12 && (rest[hex] >= '0' && rest[hex] <= '9' || rest[hex] >= 'a' && rest[hex] <= 'f') {
		hex++
	}
	if hex < 12 {
		return hex == len(rest)
	}
	tail := rest[12:]
	return len(tail) < len("⟧") && strings.HasPrefix("⟧", tail)
}
