package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/tool"
)

// spy is a consumer middleware that writes down everything that passes it.
type spy struct {
	mu   sync.Mutex
	seen strings.Builder
}

func (s *spy) note(a ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintln(&s.seen, a...)
}

func (s *spy) saw(text string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Contains(s.seen.String(), text)
}

func (*spy) Name() string { return "spy" }

func (s *spy) WrapModel(_ harness.Run, next model.Provider) model.Provider {
	return model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
		b, _ := json.Marshal(req)
		s.note("request", string(b))
		return func(yield func(model.Event, error) bool) {
			for e, err := range next.Complete(ctx, req) {
				s.note(fmt.Sprintf("event %+v err %v", e, err))
				if !yield(e, err) {
					return
				}
			}
		}
	})
}

func (s *spy) WrapTool(_ harness.Run, next harness.ToolFunc) harness.ToolFunc {
	return func(ctx context.Context, c harness.Call) (string, error) {
		s.note("call", c.Name, string(c.Arguments))
		out, err := next(ctx, c)
		s.note("result", out, err)
		if h, ok := tool.AsHandoff(err); ok {
			s.note("handoff", h.Note)
		}
		return out, err
	}
}

// journalHolds reads the stored bytes the way a database dump would.
func journalHolds(t *testing.T, j harness.Journal, id, text string) bool {
	t.Helper()
	b, err := json.Marshal(mustLoad(t, j, id))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(b), text)
}

func assertClean(t *testing.T, s *spy, j harness.Journal, id string, events *[]harness.Event, mu *sync.Mutex) {
	t.Helper()
	if s.saw(raw) {
		t.Errorf("a middleware saw the raw value:\n%s", s.seen.String())
	}
	if !s.saw("[AHV]") && !s.saw("redactor") {
		t.Error("the spy saw nothing, so it proves nothing")
	}
	if journalHolds(t, j, id, raw) {
		t.Error("the journal holds the raw value")
	}
	mu.Lock()
	defer mu.Unlock()
	b, _ := json.Marshal(*events)
	if strings.Contains(string(b), raw) {
		t.Error("a sink saw the raw value")
	}
}

func recording() (context.Context, *[]harness.Event, *sync.Mutex) {
	var evs []harness.Event
	var mu sync.Mutex
	ctx := harness.WithEvents(context.Background(), func(e harness.Event) {
		mu.Lock()
		evs = append(evs, e)
		mu.Unlock()
	})
	return ctx, &evs, &mu
}

// script replies with each response in turn.
func script(resps ...model.Response) model.Provider {
	var mu sync.Mutex
	i := 0
	return model.ProviderFunc(func(context.Context, model.Request) iter.Seq2[model.Event, error] {
		mu.Lock()
		defer mu.Unlock()
		i++
		return model.Stream(resps[i-1])
	})
}

func call(name, args string) model.Response {
	return model.Response{Text: "calling " + raw, ToolCalls: []model.ToolCall{{ID: "c1", Name: name, Arguments: json.RawMessage(args)}}}
}

func TestWrapHooksNeverSeeTheRawValue(t *testing.T) {
	leak := tool.Func(tool.Spec{Name: "leak"}, func(context.Context, json.RawMessage) (string, error) {
		return "customer " + raw, nil
	})
	ctx, evs, mu := recording()
	s := &spy{}
	j := harness.NewMemJournal()
	a := &harness.Agent{Journal: j, Tools: tool.NewRegistry(leak), Hooks: []harness.Hook{s, scrubber{}},
		Provider: script(call("leak", `{"id":"`+raw+`"}`), model.Response{Text: "found " + raw})}
	in := harness.Prompt("my id is " + raw)
	in.RunID = "r"
	res, err := a.Run(ctx, in)
	if err != nil || res.Output != "found [AHV]" {
		t.Fatalf("%v %+v", err, res)
	}
	if !s.saw("customer [AHV]") {
		t.Errorf("WrapTool did not get the redacted result:\n%s", s.seen.String())
	}
	assertClean(t, s, j, "r", evs, mu)
}

func TestToolErrorsAreRedactedAndKeepTheirIdentity(t *testing.T) {
	bad := tool.Func(tool.Spec{Name: "bad"}, func(context.Context, json.RawMessage) (string, error) {
		return "", fmt.Errorf("lookup %s: %w", raw, context.DeadlineExceeded)
	})
	ctx, evs, mu := recording()
	s := &spy{}
	j := harness.NewMemJournal()
	var kept bool
	identity := wrapFunc(func(next harness.ToolFunc) harness.ToolFunc {
		return func(ctx context.Context, c harness.Call) (string, error) {
			out, err := next(ctx, c)
			kept = errors.Is(err, context.DeadlineExceeded)
			for e := err; e != nil; e = errors.Unwrap(e) {
				s.note("chain", e)
			}
			return out, err
		}
	})
	a := &harness.Agent{Journal: j, Tools: tool.NewRegistry(bad), Hooks: []harness.Hook{s, identity, scrubber{}},
		Provider: script(call("bad", `{}`), model.Response{Text: "ok"})}
	in := harness.Prompt("x")
	in.RunID = "r"
	if _, err := a.Run(ctx, in); err != nil {
		t.Fatal(err)
	}
	if !kept {
		t.Error("errors.Is no longer sees the cause")
	}
	assertClean(t, s, j, "r", evs, mu)
}

func TestProviderErrorsAreRedactedAndKeepTheirClass(t *testing.T) {
	ctx, evs, mu := recording()
	s := &spy{}
	j := harness.NewMemJournal()
	p := model.ProviderFunc(func(context.Context, model.Request) iter.Seq2[model.Event, error] {
		return model.Fail(&model.Error{Class: model.ClassInvalid, Usage: model.Usage{Input: 7}, Err: errors.New("rejected: " + raw)})
	})
	a := &harness.Agent{Journal: j, Hooks: []harness.Hook{s, scrubber{}}, Provider: p}
	in := harness.Prompt("x")
	in.RunID = "r"
	res, err := a.Run(ctx, in)
	if res == nil || res.State != harness.StateFailed || res.Class != model.ClassInvalid || res.Usage.Input != 7 {
		t.Fatalf("%v %+v", err, res)
	}
	assertClean(t, s, j, "r", evs, mu)
}

func TestHandoffNoteIsRedactedBeforeWrapTool(t *testing.T) {
	h := tool.Func(tool.Spec{Name: "h"}, func(context.Context, json.RawMessage) (string, error) {
		return "", &tool.Handoff{Target: "next", Note: "customer " + raw}
	})
	ctx, evs, mu := recording()
	s := &spy{}
	j := harness.NewMemJournal()
	a := &harness.Agent{Journal: j, Tools: tool.NewRegistry(h), Hooks: []harness.Hook{s, scrubber{}},
		Provider: script(call("h", `{}`))}
	in := harness.Prompt("x")
	in.RunID = "r"
	res, err := a.Run(ctx, in)
	if err != nil || res.Stop != harness.StopHandoff || res.Handoff.Note != "customer [AHV]" {
		t.Fatalf("%v %+v", err, res)
	}
	assertClean(t, s, j, "r", evs, mu)
}

func TestStopDetailFromAToolIsRedactedBeforeWrapTool(t *testing.T) {
	h := tool.Func(tool.Spec{Name: "h"}, func(context.Context, json.RawMessage) (string, error) {
		return "", harness.Stop(harness.StopPolicy, "refused for "+raw)
	})
	ctx, evs, mu := recording()
	s := &spy{}
	var detail string
	see := wrapFunc(func(next harness.ToolFunc) harness.ToolFunc {
		return func(ctx context.Context, c harness.Call) (string, error) {
			out, err := next(ctx, c)
			if stop, ok := harness.AsStop(err); ok {
				detail = stop.Detail
				s.note(detail)
			}
			return out, err
		}
	})
	j := harness.NewMemJournal()
	a := &harness.Agent{Journal: j, Tools: tool.NewRegistry(h), Hooks: []harness.Hook{s, see, scrubber{}}, Provider: script(call("h", `{}`))}
	in := harness.Prompt("x")
	in.RunID = "r"
	res, err := a.Run(ctx, in)
	if err != nil || res.Stop != harness.StopPolicy || detail != "refused for [AHV]" {
		t.Fatalf("%v detail=%q %+v", err, detail, res)
	}
	assertClean(t, s, j, "r", evs, mu)
}

type inject struct{}

func (inject) Name() string { return "inject" }
func (inject) BeforeModel(_ context.Context, _ harness.Run, req *model.Request) error {
	req.Messages = append(req.Messages, model.User("retrieved "+raw))
	return nil
}

func TestTextABeforeModelHookAddsIsRedactedBeforeWrapModel(t *testing.T) {
	ctx, evs, mu := recording()
	s := &spy{}
	j := harness.NewMemJournal()
	var reached strings.Builder
	p := model.ProviderFunc(func(_ context.Context, req model.Request) iter.Seq2[model.Event, error] {
		for _, m := range req.Messages {
			reached.WriteString(m.Content)
		}
		return model.Stream(model.Response{Text: "ok"})
	})
	a := &harness.Agent{Journal: j, Hooks: []harness.Hook{inject{}, s, scrubber{}}, Provider: p}
	in := harness.Prompt("x")
	in.RunID = "r"
	if _, err := a.Run(ctx, in); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reached.String(), raw) {
		t.Error("the provider got the raw value")
	}
	assertClean(t, s, j, "r", evs, mu)
}

type streamFilter struct{ scrubber }

func (streamFilter) WrapModel(_ harness.Run, next model.Provider) model.Provider {
	return model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
		return func(yield func(model.Event, error) bool) {
			for e, err := range next.Complete(ctx, req) {
				if d, ok := e.(model.TextDelta); ok {
					e = model.TextDelta{Text: strings.ReplaceAll(d.Text, raw, "[AHV]")}
				}
				if !yield(e, err) {
					return
				}
			}
		}
	})
}

// A redactor that filters the stream itself sits in the ring, inside every other
// WrapModel, whatever order the hooks are listed in, and keeps streaming live.
func TestStreamFilterRunsInsideTheRingAndStaysLive(t *testing.T) {
	ctx, evs, mu := recording()
	s := &spy{}
	j := harness.NewMemJournal()
	gate := make(chan struct{})
	live := false
	p := model.ProviderFunc(func(context.Context, model.Request) iter.Seq2[model.Event, error] {
		return func(yield func(model.Event, error) bool) {
			if !yield(model.TextDelta{Text: "a " + raw + " "}, nil) {
				return
			}
			select {
			case <-gate:
				live = true
			default:
			}
			yield(model.Finish{Reason: model.FinishStop}, nil)
		}
	})
	ctx = harness.WithEvents(ctx, func(e harness.Event) {
		if e.Kind == harness.KindDelta {
			close(gate)
		}
	})
	a := &harness.Agent{Journal: j, Hooks: []harness.Hook{s, streamFilter{}}, Provider: p}
	in := harness.Prompt("x")
	in.RunID = "r"
	if _, err := a.Run(ctx, in); err != nil {
		t.Fatal(err)
	}
	if !live {
		t.Error("the delta did not reach the sink before the provider finished")
	}
	assertClean(t, s, j, "r", evs, mu)
}

func TestAStreamFiltersProviderErrorIsRedacted(t *testing.T) {
	ctx, evs, mu := recording()
	s := &spy{}
	j := harness.NewMemJournal()
	p := model.ProviderFunc(func(context.Context, model.Request) iter.Seq2[model.Event, error] {
		return model.Fail(errors.New("rejected: " + raw))
	})
	a := &harness.Agent{Journal: j, Provider: p, Hooks: []harness.Hook{s, streamFilter{}}}
	in := harness.Prompt("x")
	in.RunID = "r"
	if res, err := a.Run(ctx, in); err == nil || res.State != harness.StateFailed {
		t.Fatalf("%v %+v", err, res)
	}
	assertClean(t, s, j, "r", evs, mu)
}

type failing struct {
	kind harness.Kind
	mu   sync.Mutex
	on   bool
}

func (*failing) Name() string { return "failing" }
func (f *failing) Redact(_ context.Context, k harness.Kind, s string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if k == f.kind {
		f.on = true
	}
	if f.on {
		return "", errors.New("pattern service down")
	}
	return strings.ReplaceAll(s, raw, "[AHV]"), nil
}

func TestRedactorFailureInTheToolRingDiscardsTheResultAndEndsTheRun(t *testing.T) {
	ran := 0
	leak := tool.Func(tool.Spec{Name: "leak"}, func(context.Context, json.RawMessage) (string, error) {
		ran++
		return "customer " + raw, nil
	})
	ctx, evs, mu := recording()
	s := &spy{}
	j := harness.NewMemJournal()
	a := &harness.Agent{Journal: j, Tools: tool.NewRegistry(leak), Hooks: []harness.Hook{s, &failing{kind: harness.KindToolDone}},
		Provider: script(call("leak", `{}`), model.Response{Text: "never"})}
	in := harness.Prompt("x")
	in.RunID = "r"
	res, err := a.Run(ctx, in)
	if err == nil || res == nil || res.State != harness.StateFailed {
		t.Fatalf("%v %+v", err, res)
	}
	if ran != 1 {
		t.Fatalf("tool ran %d times", ran)
	}
	assertClean(t, s, j, "r", evs, mu)
	stored := mustLoad(t, j, "r")
	var done bool
	for _, e := range stored {
		done = done || e.Kind == harness.KindToolDone
	}
	if !done {
		t.Error("the effect happened and the journal does not say so")
	}
	again, err := harness.Fold(stored)
	if err != nil || again.ModelCalls != 1 {
		t.Fatalf("%v %+v", err, again)
	}
}

func TestRedactorFailureInTheModelRingDiscardsTheReplyAndKeepsItsUsage(t *testing.T) {
	ctx, evs, mu := recording()
	s := &spy{}
	j := harness.NewMemJournal()
	a := &harness.Agent{Journal: j, Hooks: []harness.Hook{s, &failing{kind: harness.KindModelDone}, retryAlways{}},
		Provider: script(model.Response{Text: "reply " + raw, Usage: model.Usage{Input: 9, Output: 3}}, model.Response{Text: "never " + raw})}
	in := harness.Prompt("x")
	in.RunID = "r"
	res, err := a.Run(ctx, in)
	if err == nil || res == nil || res.State != harness.StateFailed {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Usage.Input != 9 {
		t.Fatalf("the billed reply's usage was lost: %+v", res.Usage)
	}
	if !s.saw("redactor") {
		t.Fatalf("WrapModel did not see the failure:\n%s", s.seen.String())
	}
	assertClean(t, s, j, "r", evs, mu)
}

// A provider error can quote the prompt. If the redactor dies on it, the error text
// is gone, and a retry would send more text to a worker that can no longer clean it.
func TestRedactorFailureOnAProviderErrorEndsTheRunWithoutRetrying(t *testing.T) {
	ctx, evs, mu := recording()
	s := &spy{}
	j := harness.NewMemJournal()
	calls := 0
	p := model.ProviderFunc(func(context.Context, model.Request) iter.Seq2[model.Event, error] {
		calls++
		return model.Fail(errors.New("rejected: " + raw))
	})
	a := &harness.Agent{Journal: j, Provider: p, Hooks: []harness.Hook{s, &failing{kind: harness.KindModelFailed}, retryAlways{}}}
	in := harness.Prompt("x")
	in.RunID = "r"
	res, err := a.Run(ctx, in)
	if err == nil || res == nil || res.State != harness.StateFailed {
		t.Fatalf("%v %+v", err, res)
	}
	if calls != 1 {
		t.Fatalf("a sealed worker called the model %d times", calls)
	}
	assertClean(t, s, j, "r", evs, mu)
}

type retryAlways struct{}

func (retryAlways) Name() string { return "retry" }
func (retryAlways) Recover(context.Context, harness.Run, harness.Failure) harness.Recovery {
	return harness.Recovery{Retry: true}
}

type wrapFunc func(harness.ToolFunc) harness.ToolFunc

func (wrapFunc) Name() string                                                     { return "wrap" }
func (f wrapFunc) WrapTool(_ harness.Run, next harness.ToolFunc) harness.ToolFunc { return f(next) }
