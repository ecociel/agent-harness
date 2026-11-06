package harness_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/tool"
)

// An operator who knows a once-only call never happened asks for it to run again.
func TestARerunAnswerRunsTheCallAgainUnderItsKey(t *testing.T) {
	ctx := context.Background()
	inner := harness.NewMemJournal()
	w := newWorld()
	build := func(j harness.Journal) *harness.Agent {
		return &harness.Agent{Journal: j, Provider: w.provider("pay"), Tools: tool.NewRegistry(w.tool("pay", tool.EffectOnce))}
	}
	in := harness.Prompt("pay")
	in.RunID = "rerun"
	// The fifth append is tool.started: it lands, and the process dies before the tool runs.
	if _, err := build(&crashJournal{Journal: inner, at: 5, after: true}).Run(ctx, in); !errors.Is(err, harness.ErrDrained) {
		t.Fatalf("want a crash, got %v", err)
	}
	res, err := build(inner).Run(ctx, harness.Resume("rerun"))
	if err != nil || res.State != harness.StateSuspended || res.Waits[0].Kind != harness.WaitUnknownOutcome {
		t.Fatalf("%v %+v", err, res)
	}
	res, err = build(inner).Run(ctx, harness.Resume("rerun", harness.Signal{Wait: res.Waits[0].ID, Rerun: true, By: "op"}))
	if err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if len(w.effect["pay"]) != 1 {
		t.Fatalf("pay ran %v", w.effect["pay"])
	}
	var attempts []int
	var result string
	for _, e := range mustLoad(t, inner, "rerun") {
		switch e.Kind {
		case harness.KindToolStarted:
			attempts = append(attempts, e.Attempt)
		case harness.KindToolDone:
			result = e.Text
		}
	}
	if fmt.Sprint(attempts) != "[1 2]" || result != "pay ok" {
		t.Fatalf("attempts %v, result %q", attempts, result)
	}
}

// tagger allows every call and notes why.
type tagger struct{}

func (tagger) Name() string { return "tagger" }
func (tagger) BeforeTool(context.Context, harness.Run, harness.Call) harness.Decision {
	d := harness.Allow()
	d.Tags = map[string]string{"risk": "write", "reason": "secret 42"}
	return d
}

type scrub42 struct{}

func (scrub42) Name() string { return "scrub42" }
func (scrub42) Redact(_ context.Context, _ harness.Kind, s string) (string, error) {
	return strings.ReplaceAll(s, "42", "[n]"), nil
}

func TestDecisionTagsAreRecordedRedactedAndKeptFromTheModel(t *testing.T) {
	j := harness.NewMemJournal()
	w := newWorld()
	rq := &requests{}
	a := &harness.Agent{Journal: j, Provider: rq.wrap(w.provider("pay")), Tools: tool.NewRegistry(w.tool("pay", tool.EffectOnce)),
		Hooks: []harness.Hook{tagger{}, scrub42{}}}
	in := harness.Prompt("pay")
	in.RunID = "tags"
	if res, err := a.Run(context.Background(), in); err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	var tags map[string]string
	for _, e := range mustLoad(t, j, "tags") {
		if e.Kind == harness.KindToolGated {
			tags = e.Decision.Tags
		}
	}
	if got := fmt.Sprint(tags); got != "map[reason:secret [n] risk:write]" {
		t.Fatalf("tool.gated tags %s", got)
	}
	if strings.Contains(fmt.Sprint(rq.calls), "secret") {
		t.Fatalf("the model saw a tag: %v", rq.calls)
	}
}

// A cut stream and its retry both stream text. Each delta says which attempt it
// belongs to, so a sink can drop the superseded one.
func TestDeltasCarryTheirAttempt(t *testing.T) {
	var mu sync.Mutex
	var deltas []string
	ctx := harness.WithEvents(context.Background(), func(e harness.Event) {
		if e.Kind == harness.KindDelta {
			mu.Lock()
			deltas = append(deltas, fmt.Sprintf("%d/%d:%s", e.Turn, e.Attempt, e.Text))
			mu.Unlock()
		}
	})
	calls := 0
	p := model.ProviderFunc(func(context.Context, model.Request) iter.Seq2[model.Event, error] {
		calls++
		if calls == 1 {
			return func(yield func(model.Event, error) bool) {
				if yield(model.TextDelta{Text: "cut"}, nil) {
					yield(nil, &model.Error{Class: model.ClassRateLimit, Err: errors.New("429")})
				}
			}
		}
		return model.Stream(model.Response{Text: "whole"})
	})
	a := &harness.Agent{Provider: p, Hooks: []harness.Hook{retryOnce{}}}
	if res, err := a.Run(ctx, harness.Prompt("hi")); err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
	if got := fmt.Sprint(deltas); got != "[1/1:cut 1/2:whole]" {
		t.Fatalf("deltas %s", got)
	}
}

func TestModelDoneRecordsTheTimeToTheFirstToken(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply model.Response
		wait  time.Duration
	}{
		{"streamed", model.Response{Text: "hello"}, 30 * time.Millisecond},
		{"nothing streamed", model.Response{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := harness.NewMemJournal()
			p := model.ProviderFunc(func(context.Context, model.Request) iter.Seq2[model.Event, error] {
				return func(yield func(model.Event, error) bool) {
					time.Sleep(30 * time.Millisecond)
					for e, err := range model.Stream(tc.reply) {
						if !yield(e, err) {
							return
						}
					}
				}
			})
			in := harness.Prompt("hi")
			in.RunID = "first"
			if _, err := (&harness.Agent{Journal: j, Provider: p}).Run(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			for _, e := range mustLoad(t, j, "first") {
				if e.Kind != harness.KindModelDone {
					continue
				}
				if tc.wait == 0 && e.FirstToken != 0 {
					t.Fatalf("FirstToken %v for a reply with no tokens", e.FirstToken)
				}
				if tc.wait > 0 && (e.FirstToken < tc.wait || e.FirstToken > e.Duration) {
					t.Fatalf("FirstToken %v, Duration %v", e.FirstToken, e.Duration)
				}
			}
		})
	}
}
