package retry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/retry"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
)

func failing(n *int, until int, class model.ErrorClass) *fake.Provider {
	return fake.NewFunc(func(model.Request) (model.Response, error) {
		*n++
		if *n <= until || until < 0 {
			return model.Response{}, &model.Error{Class: class, Err: errors.New("no")}
		}
		return fake.Text("finally"), nil
	})
}

func TestRetryThenSucceed(t *testing.T) {
	n := 0
	a := &harness.Agent{Provider: failing(&n, 2, model.ClassTransient), Hooks: []harness.Hook{&retry.Retry{Base: time.Millisecond}}}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil || res.Output != "finally" || n != 3 {
		t.Fatalf("res=%v err=%v n=%d", res, err, n)
	}
}

func TestItGivesUpAfterItsAttempts(t *testing.T) {
	n := 0
	a := &harness.Agent{Provider: failing(&n, -1, model.ClassTransient), Hooks: []harness.Hook{&retry.Retry{Attempts: 2, Base: time.Millisecond}}}
	if _, err := a.Run(context.Background(), harness.Prompt("x")); err == nil || n != 2 {
		t.Fatalf("err=%v n=%d", err, n)
	}
}

func TestOnlyFailuresAnotherTryCanFixAreRetried(t *testing.T) {
	for _, class := range []model.ErrorClass{model.ClassAuth, model.ClassInvalid, model.ClassContextLength} {
		n := 0
		a := &harness.Agent{Provider: failing(&n, 1, class), Hooks: []harness.Hook{&retry.Retry{Base: time.Millisecond}}}
		if _, err := a.Run(context.Background(), harness.Prompt("x")); err == nil || n != 1 {
			t.Errorf("%s: err=%v n=%d", class, err, n)
		}
	}
	n := 0
	h := &retry.Retry{Base: time.Millisecond, Retryable: func(f harness.Failure) bool { return f.Class == model.ClassAuth }}
	a := &harness.Agent{Provider: failing(&n, 1, model.ClassAuth), Hooks: []harness.Hook{h}}
	if res, err := a.Run(context.Background(), harness.Prompt("x")); err != nil || !res.OK() {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	h := &retry.Retry{Attempts: 100, Base: 100 * time.Millisecond, Max: time.Second}
	for attempt, floor := range map[int]time.Duration{1: 100 * time.Millisecond, 2: 200 * time.Millisecond, 3: 400 * time.Millisecond, 50: time.Second} {
		rec := h.Recover(context.Background(), harness.Run{}, harness.Failure{Class: model.ClassTransient, Attempt: attempt})
		if !rec.Retry || rec.After < floor || rec.After > floor+floor/2 {
			t.Errorf("attempt %d: %+v", attempt, rec)
		}
	}
}

// The attempts are in the journal, so a new worker continues the count.
func TestACrashDoesNotResetTheCount(t *testing.T) {
	n := 0
	j := harness.NewMemJournal()
	build := func() *harness.Agent {
		return &harness.Agent{Name: "a", Journal: j, Provider: failing(&n, -1, model.ClassTransient),
			Hooks: []harness.Hook{&retry.Retry{Attempts: 3, Base: time.Millisecond}}}
	}
	in := harness.Prompt("x")
	in.RunID = "r"
	if _, err := build().Run(context.Background(), in); err == nil || n != 3 {
		t.Fatalf("err=%v n=%d", err, n)
	}
	res, err := build().Run(context.Background(), harness.Resume("r"))
	if res == nil || res.State != harness.StateFailed || n != 3 {
		t.Fatalf("a finished run paid again: %v %+v, n=%d", err, res, n)
	}
}

func TestFallbackGetsOneTryAndOnlyThatCall(t *testing.T) {
	n := 0
	backup := fake.Script(fake.Call("missing", nil), fake.Text("from backup"))
	primary := fake.NewFunc(func(req model.Request) (model.Response, error) {
		n++
		if n <= 2 {
			return model.Response{}, &model.Error{Class: model.ClassTransient, Err: errors.New("down")}
		}
		return fake.Text("primary again"), nil
	})
	a := &harness.Agent{Model: "main", Provider: retry.Fallback(primary, backup, "backup"),
		Hooks: []harness.Hook{&retry.Retry{Attempts: 2, Base: time.Millisecond, FallbackModel: "backup"}}}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil || res.Output != "primary again" || n != 3 {
		t.Fatalf("%v %+v n=%d", err, res, n)
	}
	if reqs := backup.Requests(); len(reqs) != 1 || reqs[0].Model != "backup" {
		t.Fatalf("%+v", reqs)
	}
}

func TestAFallbackThatFailsEndsTheRun(t *testing.T) {
	n, m := 0, 0
	a := &harness.Agent{Provider: retry.Fallback(failing(&n, -1, model.ClassTransient), failing(&m, -1, model.ClassTransient), "backup"),
		Hooks: []harness.Hook{&retry.Retry{Attempts: 2, Base: time.Millisecond, FallbackModel: "backup"}}}
	if _, err := a.Run(context.Background(), harness.Prompt("x")); err == nil || n != 2 || m != 1 {
		t.Fatalf("err=%v primary=%d fallback=%d", err, n, m)
	}
}

type scrub struct{}

func (scrub) Name() string { return "scrub" }
func (scrub) Redact(_ context.Context, _ harness.Kind, s string) (string, error) {
	if s == "the secret" {
		return "[gone]", nil
	}
	return s, nil
}

type seen struct{ text *string }

func (seen) Name() string { return "seen" }
func (s seen) AfterModel(_ context.Context, _ harness.Run, r model.Response) (model.Response, error) {
	*s.text = r.Text
	return r, nil
}

// The reason fallback is a provider: a hook must never meet what a redactor would
// have taken out, wherever the reply came from.
func TestAFallbackReplyPassesTheRedactionRing(t *testing.T) {
	n := 0
	var got string
	a := &harness.Agent{Provider: retry.Fallback(failing(&n, -1, model.ClassTransient), fake.Script(fake.Text("the secret")), "backup"),
		Hooks: []harness.Hook{&retry.Retry{Attempts: 1, FallbackModel: "backup"}, scrub{}, seen{&got}}}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil || res.Output != "[gone]" || got != "[gone]" {
		t.Fatalf("%v %+v, the hook saw %q", err, res, got)
	}
}
