package guard_test

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/guard"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/tool"
)

// mail is bounded (at most 48 bytes), so a window of 64 holds any match.
var mail = guard.R("email", `[a-z0-9._]{1,20}@[a-z0-9.]{1,20}\.[a-z]{2,6}`)

const reply = "Write to anna.meier@example.ch or bob@corp.com today. " +
	"The quick brown fox jumps over the lazy dog, again and again, until the text is long. " +
	"Then carol@x.org and last dave99@mail.example.net close it. Thanks for reading."

// chunked streams text in pieces of n bytes, so a match straddles chunks.
func chunked(text string, n int) model.Provider {
	return model.ProviderFunc(func(context.Context, model.Request) iter.Seq2[model.Event, error] {
		return func(yield func(model.Event, error) bool) {
			for i := 0; i < len(text); i += n {
				if !yield(model.TextDelta{Text: text[i:min(i+n, len(text))]}, nil) {
					return
				}
			}
			yield(model.Finish{Reason: model.FinishStop}, nil)
		}
	})
}

// deltas runs one agent and returns the concatenated deltas and the final answer.
func deltas(t *testing.T, p model.Provider, hooks ...harness.Hook) (streamed string, out string) {
	t.Helper()
	var mu sync.Mutex
	var sb strings.Builder
	ctx := harness.WithEvents(context.Background(), func(e harness.Event) {
		if e.Kind == harness.KindDelta {
			mu.Lock()
			sb.WriteString(e.Text)
			mu.Unlock()
		}
	})
	a := &harness.Agent{Provider: p, Hooks: hooks}
	res, err := a.Run(ctx, harness.Prompt("hi"))
	if err != nil || res.Stop != harness.StopCompleted {
		t.Fatalf("%v %+v", err, res)
	}
	return sb.String(), res.Output
}

func TestStreamDeltasEqualTheRedactedWholeReply(t *testing.T) {
	red, err := guard.Stream([]guard.Rule{mail}, 64)
	if err != nil {
		t.Fatal(err)
	}
	want := guard.Redactor{mail}.Redact
	whole, _ := want(context.Background(), harness.KindDelta, reply)
	if strings.Contains(whole, "@") {
		t.Fatal(whole)
	}
	for _, n := range []int{1, 2, 3, 5, 7, 16, 64, 500} {
		streamed, out := deltas(t, chunked(reply, n), red)
		if streamed != whole || out != whole {
			t.Fatalf("chunk %d\nstreamed %q\nout      %q\nwant     %q", n, streamed, out, whole)
		}
	}
}

func TestStreamIsLive(t *testing.T) {
	red, _ := guard.Stream([]guard.Rule{mail}, 64)
	gate := make(chan struct{})
	var once sync.Once
	live := false
	p := model.ProviderFunc(func(context.Context, model.Request) iter.Seq2[model.Event, error] {
		return func(yield func(model.Event, error) bool) {
			for i := 0; i < len(reply); i += 4 {
				if !yield(model.TextDelta{Text: reply[i:min(i+4, len(reply))]}, nil) {
					return
				}
			}
			select {
			case <-gate:
				live = true
			default:
			}
			yield(model.Finish{Reason: model.FinishStop}, nil)
		}
	})
	ctx := harness.WithEvents(context.Background(), func(e harness.Event) {
		if e.Kind == harness.KindDelta {
			once.Do(func() { close(gate) })
		}
	})
	a := &harness.Agent{Provider: p, Hooks: []harness.Hook{red}}
	if _, err := a.Run(ctx, harness.Prompt("x")); err != nil {
		t.Fatal(err)
	}
	if !live {
		t.Fatal("no delta reached the sink before the provider finished")
	}
}

func TestStreamNeverEmitsAPartialMatch(t *testing.T) {
	red, _ := guard.Stream([]guard.Rule{mail}, 64)
	var seen []string
	p := chunked(strings.Repeat("x", 70)+" see anna.meier@example.ch and more text after it", 1)
	ctx := harness.WithEvents(context.Background(), func(e harness.Event) {
		if e.Kind == harness.KindDelta {
			seen = append(seen, e.Text)
		}
	})
	a := &harness.Agent{Provider: p, Hooks: []harness.Hook{red}}
	if _, err := a.Run(ctx, harness.Prompt("x")); err != nil {
		t.Fatal(err)
	}
	for _, s := range seen {
		if strings.ContainsAny(s, "@") || strings.Contains(s, "anna") {
			t.Fatalf("partial match emitted: %q", seen)
		}
	}
}

func TestStreamRedactsToolCallArguments(t *testing.T) {
	red, _ := guard.Stream([]guard.Rule{mail}, 64)
	p := model.ProviderFunc(func(context.Context, model.Request) iter.Seq2[model.Event, error] {
		return func(yield func(model.Event, error) bool) {
			yield(model.ToolCallDelta{Index: 0, ID: "c1", Name: "send", Arguments: `{"to":"anna.me`}, nil)
			yield(model.ToolCallDelta{Index: 0, Arguments: `ier@example.ch","n":3}`}, nil)
			yield(model.Finish{Reason: model.FinishToolCalls}, nil)
		}
	})
	resp, err := model.Collect(red.(harness.ModelMiddleware).WrapModel(harness.Run{}, p).Complete(context.Background(), model.Request{}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resp.ToolCalls[0].Arguments); got != `{"to":"[redacted:email]","n":3}` {
		t.Fatal(got)
	}
}

func TestStreamRejectsARuleThatCanExceedTheWindow(t *testing.T) {
	for name, c := range map[string]struct {
		rules  []guard.Rule
		window int
	}{
		"unbounded":      {[]guard.Rule{guard.R("x", `a+`)}, 64},
		"longer than it": {[]guard.Rule{mail}, 47},
		"negative":       {[]guard.Rule{mail}, -1},
	} {
		if _, err := guard.Stream(c.rules, c.window); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := guard.Stream([]guard.Rule{mail}, 48); err != nil {
		t.Fatal(err)
	}
}

func TestStreamAsAPlainRedactor(t *testing.T) {
	red, _ := guard.Stream([]guard.Rule{mail}, 64)
	got, err := red.Redact(context.Background(), harness.KindToolDone, "x a@b.ch y")
	if err != nil || got != "x [redacted:email] y" {
		t.Fatal(got, err)
	}
}

func TestEveryShippedRuleIsBounded(t *testing.T) {
	for name, rules := range map[string][]guard.Rule{"PII": guard.PII(), "Secrets": guard.Secrets()} {
		if _, err := guard.Stream(rules, 0); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		for _, r := range rules {
			if _, err := guard.Stream([]guard.Rule{r}, 0); err != nil {
				t.Errorf("%s/%s: %v", name, r.Name, err)
			}
		}
	}
	p := guard.Pseudonymizer{Rules: guard.PII(), Key: []byte("k"), Vault: guard.NewMemVault()}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

// argsRaw has a duplicate key, a case-folded duplicate, unusual whitespace, a kept
// escape, an unusual number and a secret inside a nested array.
const argsRaw = "{\"a\":1,\"a\":2,\"A\" :\t3,\n  \"list\": [ \"plain\\u00e9\" , [\"key sk-abcdefghijklmnop1234 end\"] ],\"n\":1.50e+2 }"

const argsSecret = "key sk-abcdefghijklmnop1234 end"

// toolOnce asks for one call to send with raw, streamed in pieces of three bytes,
// then answers.
func toolOnce(raw string) model.Provider {
	var calls int
	return model.ProviderFunc(func(context.Context, model.Request) iter.Seq2[model.Event, error] {
		calls++
		first := calls == 1
		return func(yield func(model.Event, error) bool) {
			if !first {
				yield(model.TextDelta{Text: "done"}, nil)
				yield(model.Finish{Reason: model.FinishStop}, nil)
				return
			}
			for i := 0; i < len(raw); i += 3 {
				d := model.ToolCallDelta{Index: 0, Arguments: raw[i:min(i+3, len(raw))]}
				if i == 0 {
					d.ID, d.Name = "c1", "send"
				}
				if !yield(d, nil) {
					return
				}
			}
			yield(model.Finish{Reason: model.FinishToolCalls}, nil)
		}
	})
}

func TestRedactionKeepsTheArgumentBytes(t *testing.T) {
	ctx := context.Background()
	stream, err := guard.Stream(guard.Secrets(), 0)
	if err != nil {
		t.Fatal(err)
	}
	p := guard.Pseudonymizer{Rules: guard.Secrets(), Key: []byte("k"), Vault: guard.NewMemVault()}
	tok, _ := p.Redact(ctx, harness.KindDelta, argsSecret)
	for name, c := range map[string]struct {
		hook harness.Hook
		with string
	}{
		"stream":       {stream, "key [redacted:api_key] end"},
		"pseudonymize": {p, tok},
		"buffered":     {guard.Redactor(guard.Secrets()), "key [redacted:api_key] end"},
	} {
		var got string
		a := &harness.Agent{Provider: toolOnce(argsRaw), Tools: tool.NewRegistry(recorder(&got)), Hooks: []harness.Hook{c.hook}}
		res, err := a.Run(ctx, harness.Prompt("x"))
		if err != nil || res.Stop != harness.StopCompleted {
			t.Fatalf("%s: %v %+v", name, err, res)
		}
		want := strings.Replace(argsRaw, argsSecret, c.with, 1)
		if c.with == argsSecret || want == argsRaw || got != want {
			t.Errorf("%s\ngot  %q\nwant %q", name, got, want)
		}
	}
}

func TestShippedRuleWindowsStaySmall(t *testing.T) {
	for name, c := range map[string]struct {
		rules   []guard.Rule
		ceiling int
	}{
		"Secrets": {guard.Secrets(), 700},
		"PII":     {guard.PII(), 360},
	} {
		if _, err := guard.Stream(c.rules, c.ceiling); err != nil {
			t.Errorf("%s: the default window grew past %d: %v", name, c.ceiling, err)
		}
	}
}

// A case-insensitive k also matches the Kelvin sign, which takes three bytes, so a
// window must hold the widest fold of every rune, not one byte per ASCII letter.
func TestStreamHoldsTheWidestFold(t *testing.T) {
	red, err := guard.Stream([]guard.Rule{guard.R("kw", `(?i)kkkk`)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{1, 2, 3, 4, 5} {
		streamed, out := deltas(t, chunked("some text \u212a\u212a\u212a\u212a and more text after it", n), red)
		if want := "some text [redacted:kw] and more text after it"; streamed != want || out != want {
			t.Fatalf("chunk %d: streamed %q out %q", n, streamed, out)
		}
	}
}
