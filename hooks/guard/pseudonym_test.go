package guard_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/guard"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
)

var unbounded = []guard.Rule{guard.R("email", `[a-z0-9._]+@[a-z0-9.]+\.[a-z]{2,}`)}

const addr = "anna.meier@example.ch"

var tokenRe = regexp.MustCompile(`⟦email:[0-9a-f]{12}⟧`)

func pseudo(v guard.Vault) guard.Pseudonymizer {
	return guard.Pseudonymizer{Rules: []guard.Rule{mail}, Key: []byte("k1"), Vault: v, Window: 64}
}

// run starts one run and returns what each place saw.
type seen struct {
	requests []model.Request
	journal  string
	sinks    string
	output   string
}

func runWith(t *testing.T, p guard.Pseudonymizer, prov *fake.Provider, runID, prompt string, more ...harness.Hook) seen {
	t.Helper()
	j := harness.NewMemJournal()
	var mu sync.Mutex
	var sinks []harness.Event
	ctx := harness.WithEvents(context.Background(), func(e harness.Event) {
		mu.Lock()
		sinks = append(sinks, e)
		mu.Unlock()
	})
	a := &harness.Agent{Name: "a", Journal: j, Provider: prov, Hooks: append([]harness.Hook{p}, more...)}
	in := harness.Prompt(prompt)
	in.RunID = runID
	res, err := a.Run(ctx, in)
	if err != nil || res.Stop != harness.StopCompleted {
		t.Fatalf("%v %+v", err, res)
	}
	evs, _ := j.Load(context.Background(), runID)
	jb, _ := json.Marshal(evs)
	sb, _ := json.Marshal(sinks)
	return seen{prov.Requests(), string(jb), string(sb), res.Output}
}

func echo() *fake.Provider {
	return fake.NewFunc(func(req model.Request) (model.Response, error) {
		return fake.Text("I will write to " + strings.TrimPrefix(model.LastUser(req.Messages), "mail ") + " now"), nil
	})
}

func TestPseudonymizerShowsEveryoneTokensOnly(t *testing.T) {
	v := guard.NewMemVault()
	s := runWith(t, pseudo(v), echo(), "r1", "mail "+addr)
	tok := tokenRe.FindString(s.output)
	if tok == "" || s.output != "I will write to "+tok+" now" {
		t.Fatalf("output %q", s.output)
	}
	reqs, _ := json.Marshal(s.requests)
	for name, text := range map[string]string{"request": string(reqs), "journal": s.journal, "sinks": s.sinks, "output": s.output} {
		if strings.Contains(text, addr) || strings.Contains(text, "anna.meier") {
			t.Errorf("%s holds the address", name)
		}
		if !strings.Contains(text, tok) {
			t.Errorf("%s lacks the token", name)
		}
	}
	got, ok, err := v.Get(context.Background(), tok)
	if err != nil || !ok || got != addr {
		t.Fatalf("vault: %q %v %v", got, ok, err)
	}
}

func TestPseudonymizerGivesTheSameTokenAcrossRunsAndNewOnesForNewKeys(t *testing.T) {
	v := guard.NewMemVault()
	a := runWith(t, pseudo(v), echo(), "r1", "mail "+addr).output
	b := runWith(t, pseudo(v), echo(), "r2", "mail "+addr).output
	if a != b {
		t.Fatalf("%q != %q", a, b)
	}
	other := pseudo(v)
	other.Key = []byte("k2")
	if c := runWith(t, other, echo(), "r3", "mail "+addr).output; c == a {
		t.Fatal("a different key gave the same token")
	}
	d := runWith(t, pseudo(v), echo(), "r4", "mail bob@corp.com").output
	if d == a {
		t.Fatal("two values share a token")
	}
}

type brokenVault struct{ guard.Vault }

func (brokenVault) Put(context.Context, string, string) error {
	return errors.New("vault down: " + addr)
}

func TestPseudonymizerFailsClosedWhenTheVaultFails(t *testing.T) {
	a := &harness.Agent{Provider: echo(), Hooks: []harness.Hook{pseudo(brokenVault{guard.NewMemVault()})}}
	res, err := a.Run(context.Background(), harness.Prompt("mail "+addr))
	if err == nil && (res == nil || res.Stop == harness.StopCompleted) {
		t.Fatalf("the run went on: %+v", res)
	}
	text := errString(err)
	if res != nil {
		text += res.Output + res.Detail
	}
	if strings.Contains(text, addr) {
		t.Fatal("the address leaked into the failure")
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestPseudonymizerStreamsLive(t *testing.T) {
	v := guard.NewMemVault()
	p := pseudo(v)
	streamed, out := deltas(t, chunked("Write to "+addr+" "+strings.Repeat("and so on ", 20)+"bye", 3), p)
	if streamed != out || strings.Contains(streamed, "anna") || !tokenRe.MatchString(streamed) {
		t.Fatalf("%q", streamed)
	}
}

func TestPseudonymizerWithoutAWindowHoldsTheReplyForUnboundedRules(t *testing.T) {
	v := guard.NewMemVault()
	p := guard.Pseudonymizer{Rules: unbounded, Key: []byte("k"), Vault: v}
	_, out := deltas(t, chunked("a "+addr+" b", 2), p)
	if !tokenRe.MatchString(out) || strings.Contains(out, "anna") {
		t.Fatal(out)
	}
	if err := (guard.Pseudonymizer{Rules: unbounded, Key: []byte("k"), Vault: v, Window: 8}).Validate(); err == nil {
		t.Fatal("an unbounded rule with a window was accepted")
	}
}

func TestMemVaultKeepsTheLatestValue(t *testing.T) {
	v := guard.NewMemVault()
	ctx := context.Background()
	for _, value := range []string{"a", "a", "b"} {
		if err := v.Put(ctx, "t", value); err != nil {
			t.Fatal(err)
		}
	}
	if got, ok, _ := v.Get(ctx, "t"); !ok || got != "b" {
		t.Fatalf("%q %v", got, ok)
	}
	if _, ok, _ := v.Get(ctx, "nope"); ok {
		t.Fatal("found an unknown token")
	}
}

func TestTokenizeMintsTheTokenTheRingDoes(t *testing.T) {
	ctx := context.Background()
	v := guard.NewMemVault()
	d, err := guard.NewDictionary([]guard.Term{{Text: "Anna Müller", Class: "person", Key: "party/4711", Value: "Anna Müller"}}, guard.TermOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := guard.Pseudonymizer{Finders: []guard.Finder{d}, Key: []byte("k"), Vault: v}
	tok, err := p.Tokenize(ctx, "person", "party/4711", "Anna Müller")
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := p.Redact(ctx, harness.KindToolDone, "Dear Anna Mueller"); out != "Dear "+tok {
		t.Fatalf("ring %q, Tokenize %q", out, tok)
	}
	if out, _ := p.Redact(ctx, harness.KindToolDone, "Dear "+tok); out != "Dear "+tok {
		t.Fatalf("a minted token was redacted again: %q", out)
	}
	renamed, err := p.Tokenize(ctx, "person", "party/4711", "Anna Keller")
	if err != nil || renamed != tok {
		t.Fatalf("a rename changed the token: %q %v", renamed, err)
	}
	if got, _ := guard.RevealText(ctx, v, "Dear "+tok); got != "Dear Anna Keller" {
		t.Fatalf("revealed %q", got)
	}
	if other, _ := p.Tokenize(ctx, "person", "party/4712", "Anna Keller"); other == tok {
		t.Fatal("two keys share a token")
	}
}

func TestTokenizeRefusesWhatCannotBeAToken(t *testing.T) {
	ctx := context.Background()
	p := guard.Pseudonymizer{Key: []byte("k"), Vault: guard.NewMemVault()}
	for _, class := range []string{"", "a b", "a⟧", strings.Repeat("x", 65)} {
		if tok, err := p.Tokenize(ctx, class, "k", "v"); err == nil {
			t.Errorf("class %q gave %q", class, tok)
		}
	}
	if _, err := (guard.Pseudonymizer{Vault: guard.NewMemVault()}).Tokenize(ctx, "person", "k", "v"); err == nil {
		t.Error("no key")
	}
	if _, err := (guard.Pseudonymizer{Key: []byte("k"), Vault: brokenVault{guard.NewMemVault()}}).Tokenize(ctx, "person", "k", "v"); err == nil || strings.Contains(err.Error(), addr) {
		t.Errorf("vault failure: %v", err)
	}
	if err := (guard.Pseudonymizer{Rules: []guard.Rule{guard.R("a b", `x`)}, Key: []byte("k"), Vault: guard.NewMemVault()}).Validate(); err == nil {
		t.Error("a rule name with a space was accepted")
	}
}

// ids matches a bare run of digits, as a token's hex can be.
var ids = guard.R("id", `\b\d{6,12}\b`)

const tokenised = "⟦person:123456789012⟧"

// longToken is longer than the window of the rules it streams with.
const longToken = "⟦person/a-long-attribute-name:210987654321⟧"

func TestRedactingRedactedTextChangesNothing(t *testing.T) {
	ctx := context.Background()
	p := guard.Pseudonymizer{Rules: append(guard.PII(), ids), Key: []byte("k"), Vault: guard.NewMemVault()}
	in := "Anna " + tokenised + " has id 4711123, mail " + addr + ", phone +41 79 123 45 67, " +
		"IBAN CH93 0076 2011 6238 5295 7, card 4111 1111 1111 1111 and AHV 756.1234.5678.97."
	once, err := p.Redact(ctx, harness.KindToolDone, in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(once, tokenised) || strings.Contains(once, "4711123") || strings.Contains(once, "anna.meier") {
		t.Fatalf("once: %q", once)
	}
	twice, err := p.Redact(ctx, harness.KindToolDone, once)
	if err != nil || twice != once {
		t.Fatalf("twice: %q\nonce:  %q", twice, once)
	}
}

func TestPseudonymizerStreamsTokensAndMatchesWhole(t *testing.T) {
	p := guard.Pseudonymizer{Rules: []guard.Rule{guard.PII()[len(guard.PII())-1], ids}, Key: []byte("k"), Vault: guard.NewMemVault()}
	text := "Start " + strings.Repeat("filler text ", 3) + tokenised + " then 4711123 and " + tokenised +
		tokenised + " " + longToken + " card 4111 1111 1111 1111 " + strings.Repeat("more ", 8) + "⟦not a token " + tokenised + " end 987654"
	want, err := p.Redact(context.Background(), harness.KindDelta, text)
	if err != nil || strings.Count(want, tokenised) != 4 || !strings.Contains(want, longToken) || strings.Contains(want, "4711123") {
		t.Fatalf("%v %q", err, want)
	}
	for n := 1; n <= 40; n++ {
		streamed, out := deltas(t, chunked(text, n), p)
		if streamed != want || out != want {
			t.Fatalf("chunk %d\nstreamed %q\nwant     %q", n, streamed, want)
		}
	}
}
