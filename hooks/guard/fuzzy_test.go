package guard_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/guard"
)

func fuzzy(t *testing.T, terms ...guard.Term) *guard.Fuzzy {
	t.Helper()
	z, err := guard.NewFuzzy(terms, guard.TermOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return z
}

func TestFuzzyAllowsEditsByTermLength(t *testing.T) {
	z := fuzzy(t,
		guard.Term{Text: "Arno", Class: "person"},
		guard.Term{Text: "Mueller", Class: "person"},
		guard.Term{Text: "Bernhard Keller", Class: "person"})
	for text, want := range map[string][]string{
		"Arno and Arne":                  {"Arno"},
		"Muller signed":                  {"Muller"},
		"Mülker signed":                  {"Mülker"},
		"Mxyler signed":                  nil,
		"Bernhart Kelle signed":          {"Bernhart Kelle"},
		"Bernhard Keller, Bemhard Keler": {"Bernhard Keller", "Bemhard Keler"},
		"Bernhrt Kele signed":            nil,
		"xMueller":                       {"xMueller"},
		"zzMueller and Muellerxy":        nil,
	} {
		if got := found(t, z, text); !slices.Equal(got, want) {
			t.Errorf("%q: got %q want %q", text, got, want)
		}
	}
}

func TestFuzzyHandlesATermOfOver64Runes(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("abcdefg ", 9))
	z := fuzzy(t, guard.Term{Text: long, Class: "x"})
	typo := strings.Replace(strings.Replace(long, "c", "x", 1), "e", "", 1)
	if got := found(t, z, "see "+typo+" here"); !slices.Equal(got, []string{typo}) {
		t.Fatalf("%q", got)
	}
	worse := strings.Replace(typo, "g", "", 1)
	if got := found(t, z, "see "+worse+" here"); got != nil {
		t.Fatalf("%q", got)
	}
}

func TestFuzzyForgivesOCRConfusions(t *testing.T) {
	z := fuzzy(t,
		guard.Term{Text: "Arno", Class: "person"},
		guard.Term{Text: "Lou", Class: "person"},
		guard.Term{Text: "Jürg", Class: "person"},
		guard.Term{Text: "Schmid", Class: "person"})
	for text, want := range map[string][]string{
		"Amo wrote":     {"Amo"},
		"L0u and 1ou":   {"L0u", "1ou"},
		"Jiirg":         {"Jiirg"},
		"Schrnid":       {"Schrnid"},
		"Schrnidt GmbH": {"Schrnidt"},
		"Lau":           nil,
	} {
		if got := found(t, z, text); !slices.Equal(got, want) {
			t.Errorf("%q: got %q want %q", text, got, want)
		}
	}
}

func TestFuzzyNeverSpansALongGap(t *testing.T) {
	z := fuzzy(t, guard.Term{Text: "Bernhard Keller", Class: "person"})
	if got := found(t, z, "Bernhard"+strings.Repeat(" ", 40)+"Keller"); got != nil {
		t.Fatalf("%q", got)
	}
	if z.MaxLen() <= 0 {
		t.Fatal(z.MaxLen())
	}
}

func TestFuzzyStreamsAtEverySplit(t *testing.T) {
	z := fuzzy(t,
		guard.Term{Text: "Anna Müller", Class: "person", Key: "party/1", Value: "Anna Müller"},
		guard.Term{Text: "Arno", Class: "person", Key: "party/2", Value: "Arno"})
	p := guard.Pseudonymizer{Finders: []guard.Finder{z}, Key: []byte("k"), Vault: guard.NewMemVault()}
	text := "Amo met Anna Miiller and Arnold; Anna Muler left. " + tokenised + " Arno"
	want, err := p.Redact(context.Background(), harness.KindDelta, text)
	if err != nil || strings.Contains(want, "Anna") || strings.Contains(want, "Amo") || !strings.Contains(want, "Arnold") {
		t.Fatalf("%v %q", err, want)
	}
	for i := 1; i < len(text); i++ {
		if streamed, out := deltas(t, pieces(text[:i], text[i:]), p); streamed != want || out != want {
			t.Fatalf("split at %d\ngot  %q\nwant %q", i, streamed, want)
		}
	}
}

func TestFuzzyOfTenThousandTerms(t *testing.T) {
	terms, text, planted := tenThousand()
	z, err := guard.NewFuzzy(terms, guard.TermOptions{})
	if err != nil {
		t.Fatal(err)
	}
	misspelt := strings.ReplaceAll(text, planted[3], planted[3][:len(planted[3])-1])
	start := time.Now()
	got := found(t, z, misspelt)
	perKB := time.Since(start) / time.Duration(len(text)>>10)
	if !slices.Contains(got, planted[3][:len(planted[3])-1]) {
		t.Fatal("the misspelt name was not found")
	}
	t.Logf("%d terms, %d KB of text, %v per KB, %d spans", len(terms), len(text)>>10, perKB, len(got))
}

func BenchmarkFuzzyFind(b *testing.B) {
	terms, text, _ := tenThousand()
	z, err := guard.NewFuzzy(terms, guard.TermOptions{})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := z.Find(ctx, text); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(text)>>10), "ns/KB")
}

// Over an alphabet the normalisation leaves alone, Fuzzy finds what checking every
// whole-word window against every term finds.
func TestFuzzyFindsWhatABruteForceFinds(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	word := func() string {
		b := make([]byte, 2+rng.IntN(6))
		for i := range b {
			b[i] = "abcdefgh"[rng.IntN(8)]
		}
		return string(b)
	}
	var terms []guard.Term
	for i := range 200 {
		text := word()
		if rng.IntN(2) == 0 {
			text += " " + word()
		}
		terms = append(terms, guard.Term{Text: text, Class: "t", Key: fmt.Sprint(i)})
	}
	var words []string
	for range 3000 {
		if rng.IntN(10) == 0 {
			w := []byte(terms[rng.IntN(len(terms))].Text)
			w[rng.IntN(len(w))] = "abcdefgh"[rng.IntN(8)]
			words = append(words, string(w))
			continue
		}
		words = append(words, word())
	}
	text := strings.Join(words, " ")
	z := fuzzy(t, terms...)
	spans, err := z.Find(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range spans {
		got[fmt.Sprint(s.Start, s.End, s.Key)] = true
	}
	want := map[string]bool{}
	seen := map[string]bool{}
	isWord := func(i int) bool { return i >= 0 && i < len(text) && text[i] != ' ' }
	for _, term := range terms {
		if seen[term.Text] {
			continue
		}
		seen[term.Text] = true
		n := len(term.Text)
		k := map[bool]int{true: 2, false: 1}[n > 8]
		if n < 5 {
			k = 0
		}
		for s := 0; s < len(text); s++ {
			if !isWord(s) || isWord(s-1) {
				continue
			}
			best, bestEnd := k+1, 0
			for e := max(s+n-k, s+1); e <= min(s+n+k, len(text)); e++ {
				if !isWord(e-1) || isWord(e) {
					continue
				}
				d := levenshtein(term.Text, text[s:e])
				if d < best || d == best && absInt(e-s-n) < absInt(bestEnd-s-n) {
					best, bestEnd = d, e
				}
			}
			if best <= k {
				want[fmt.Sprint(s, bestEnd, term.Key)] = true
			}
		}
	}
	if len(want) < 100 {
		t.Fatalf("only %d matches: the test text is too thin", len(want))
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missed %s", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("extra %s", k)
		}
	}
}

func levenshtein(a, b string) int {
	row := make([]int, len(b)+1)
	for j := range row {
		row[j] = j
	}
	for i := 1; i <= len(a); i++ {
		prev := row[0]
		row[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			prev, row[j] = row[j], min(row[j]+1, row[j-1]+1, prev+cost)
		}
	}
	return row[len(b)]
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
