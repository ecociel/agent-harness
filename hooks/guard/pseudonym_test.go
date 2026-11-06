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

func TestMemVaultPutIsIdempotent(t *testing.T) {
	v := guard.NewMemVault()
	ctx := context.Background()
	if err := v.Put(ctx, "t", "a"); err != nil {
		t.Fatal(err)
	}
	if err := v.Put(ctx, "t", "a"); err != nil {
		t.Fatal(err)
	}
	if err := v.Put(ctx, "t", "b"); err == nil {
		t.Fatal("a token was bound to two values")
	}
	if _, ok, _ := v.Get(ctx, "nope"); ok {
		t.Fatal("found an unknown token")
	}
}
