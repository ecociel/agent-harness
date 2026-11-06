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
	if got := string(resp.ToolCalls[0].Arguments); got != `{"n":3,"to":"[redacted:email]"}` {
		t.Fatal(got)
	}
}

func TestStreamRejectsARuleThatCanExceedTheWindow(t *testing.T) {
	for name, c := range map[string]struct {
		rules  []guard.Rule
		window int
	}{
		"unbounded":      {guard.PII()[:1], 64},
		"longer than it": {[]guard.Rule{mail}, 47},
		"no window":      {[]guard.Rule{mail}, 0},
		"password":       {guard.Secrets(), 1000},
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
