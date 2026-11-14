package guard_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/guard"
)

// found is what a finder takes from text, as the original pieces in order.
func found(t *testing.T, f guard.Finder, text string) []string {
	t.Helper()
	spans, err := f.Find(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(spans, func(a, b guard.Span) int { return a.Start - b.Start })
	var out []string
	for _, s := range spans {
		out = append(out, text[s.Start:s.End])
	}
	return out
}

func dict(t *testing.T, o guard.TermOptions, terms ...guard.Term) *guard.Dictionary {
	t.Helper()
	d, err := guard.NewDictionary(terms, o)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDictionaryMatchesWholeWordsInAnySpelling(t *testing.T) {
	d := dict(t, guard.TermOptions{},
		guard.Term{Text: "Anna Müller", Class: "person"},
		guard.Term{Text: "Strasse", Class: "street"},
		guard.Term{Text: "Mueller", Class: "person"})
	for text, want := range map[string][]string{
		"Dear Anna Müller,":   {"Anna Müller", "Müller"},
		"ANNA MUELLER signed": {"ANNA MUELLER", "MUELLER"},
		"anna \n\t mueller":   {"anna \n\t mueller", "mueller"},
		"Anna Müller":        {"Anna Müller", "Müller"},
		"Haupt Straße 1":      {"Straße"},
		"MÜLLER":              {"MÜLLER"},
		"Annabelle Müllerin":  nil,
		"Anna" + strings.Repeat(" ", 17) + "Müller": {"Müller"},
		"xMueller and Muellerx":                     nil,
		"(Mueller)":                                 {"Mueller"},
	} {
		if got := found(t, d, text); !slices.Equal(got, want) {
			t.Errorf("%q: got %q want %q", text, got, want)
		}
	}
}

func TestDictionaryTermOptions(t *testing.T) {
	d := dict(t, guard.TermOptions{MinLen: 3},
		guard.Term{Text: "Post", Class: "org", CaseSensitive: true},
		guard.Term{Text: "Li", Class: "person"},
		guard.Term{Text: "Rüegg", Class: "person", CaseSensitive: true})
	for text, want := range map[string][]string{
		"Post and post":    {"Post"},
		"Li wrote":         nil,
		"Rueegg and rüegg": {"Rueegg"},
	} {
		if got := found(t, d, text); !slices.Equal(got, want) {
			t.Errorf("%q: got %q want %q", text, got, want)
		}
	}
	if _, err := guard.NewDictionary([]guard.Term{{Text: "x", Class: "a b"}}, guard.TermOptions{}); err == nil {
		t.Fatal("a class with a space was accepted")
	}
}

func TestDictionaryKeepsTheFirstOfTwoEqualTerms(t *testing.T) {
	ctx := context.Background()
	v := guard.NewMemVault()
	d := dict(t, guard.TermOptions{},
		guard.Term{Text: "Keller AG", Class: "org", Key: "party/1", Value: "Keller AG"},
		guard.Term{Text: "KELLER  AG", Class: "org", Key: "party/2", Value: "Keller Holding"})
	p := guard.Pseudonymizer{Finders: []guard.Finder{d}, Key: []byte("k"), Vault: v}
	out, err := p.Redact(ctx, harness.KindToolDone, "Keller AG and KELLER AG")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := guard.RevealText(ctx, v, out); got != "Keller AG and Keller AG" {
		t.Fatalf("%q -> %q", out, got)
	}
}

// Names and tokens split at every byte stream out as the whole text redacts.
func TestDictionaryStreamsAtEverySplit(t *testing.T) {
	d := dict(t, guard.TermOptions{},
		guard.Term{Text: "Anna Müller", Class: "person", Key: "party/1", Value: "Anna Müller"},
		guard.Term{Text: "Jürg", Class: "person", Key: "party/2", Value: "Jürg"},
		guard.Term{Text: "Keller AG", Class: "org", Key: "party/3", Value: "Keller AG"})
	p := guard.Pseudonymizer{Finders: []guard.Finder{d}, Key: []byte("k"), Vault: guard.NewMemVault()}
	text := "Jürg met Anna Mueller at KELLER AG; Jürgen and Annax are others. " + tokenised + " Anna Müller"
	want, err := p.Redact(context.Background(), harness.KindDelta, text)
	if err != nil || strings.Contains(want, "Anna M") || !strings.Contains(want, "Jürgen and Annax") || !strings.Contains(want, tokenised) {
		t.Fatalf("%v %q", err, want)
	}
	stream := func(cuts ...int) string {
		var parts []string
		last := 0
		for _, c := range cuts {
			parts = append(parts, text[last:c])
			last = c
		}
		parts = append(parts, text[last:])
		streamed, out := deltas(t, pieces(parts...), p)
		if streamed != out {
			t.Fatalf("streamed %q out %q", streamed, out)
		}
		return out
	}
	for i := 1; i < len(text); i++ {
		if got := stream(i); got != want {
			t.Fatalf("split at %d\ngot  %q\nwant %q", i, got, want)
		}
	}
	if got := stream(); got != want {
		t.Fatal(got)
	}
	for n := 1; n <= 8; n++ {
		if streamed, _ := deltas(t, chunked(text, n), p); streamed != want {
			t.Fatalf("chunk %d: %q", n, streamed)
		}
	}
}

// tenThousand returns 10,000 distinct names and a text of about 64 KB that holds
// one in every hundred of them.
func tenThousand() ([]guard.Term, string, []string) {
	syl := []string{"ber", "mül", "kel", "sch", "wag", "rit", "hof", "lin", "dor", "fis",
		"zim", "mei", "brau", "hub", "kra", "bau", "wolf", "neu", "stein", "hart"}
	first := []string{"Anna", "Jürg", "Luca", "Mia", "Noah", "Lea", "Elias", "Sofia", "Leon", "Emma"}
	var terms []guard.Term
	var planted []string
	for i := 0; len(terms) < 10_000; i++ {
		last := syl[i%20] + syl[i/20%20] + syl[i/400%20]
		name := first[i/8000%10] + " " + strings.ToUpper(last[:1]) + last[1:]
		terms = append(terms, guard.Term{Text: name, Class: "person", Key: fmt.Sprint("party/", i)})
		if i%100 == 7 {
			planted = append(planted, name)
		}
	}
	var b strings.Builder
	filler := "The board met on Tuesday to review the accounts and the annual report of the company. "
	for b.Len() < 64<<10 {
		b.WriteString(filler)
		b.WriteString(planted[b.Len()%len(planted)])
		b.WriteString(" signed. ")
	}
	return terms, b.String(), planted
}

func TestDictionaryOfTenThousandTerms(t *testing.T) {
	terms, text, _ := tenThousand()
	d := dict(t, guard.TermOptions{}, terms...)
	start := time.Now()
	got := found(t, d, text)
	perKB := time.Since(start) / time.Duration(len(text)>>10)
	if len(got) == 0 {
		t.Fatal("nothing found")
	}
	for _, g := range got {
		if strings.Contains(g, "signed") || strings.Contains(g, "board") {
			t.Fatalf("matched filler: %q", g)
		}
	}
	if want := strings.Count(text, " signed. "); len(got) != want {
		t.Fatalf("found %d names, the text holds %d", len(got), want)
	}
	t.Logf("%d terms, %d KB of text, %v per KB", len(terms), len(text)>>10, perKB)
	if perKB > 2*time.Millisecond {
		t.Fatalf("%v per KB", perKB)
	}
}

func BenchmarkDictionaryFind(b *testing.B) {
	terms, text, _ := tenThousand()
	d, err := guard.NewDictionary(terms, guard.TermOptions{})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := d.Find(ctx, text); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(text)>>10), "ns/KB")
}

func BenchmarkNewDictionary(b *testing.B) {
	terms, _, _ := tenThousand()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := guard.NewDictionary(terms, guard.TermOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func TestDictionaryRedactsANameNextToInvalidUTF8(t *testing.T) {
	d := dict(t, guard.TermOptions{}, guard.Term{Text: "Anna", Class: "person"})
	p := guard.Pseudonymizer{Finders: []guard.Finder{d}, Key: []byte("k"), Vault: guard.NewMemVault()}
	out, err := p.Redact(context.Background(), harness.KindToolDone, "Anna\x80 and \xffAnna")
	if err != nil || strings.Contains(out, "Anna") {
		t.Fatalf("%v %q", err, out)
	}
}
