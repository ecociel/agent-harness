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

func TestRetryThenSucceed(t *testing.T) {
	n := 0
	p := fake.NewFunc(func(model.Request) (model.Response, error) {
		n++
		if n < 3 {
			return model.Response{}, errors.New("503")
		}
		return fake.Text("finally"), nil
	})
	a := &harness.Agent{Provider: p, Hooks: []harness.Hook{&retry.Retry{Base: time.Millisecond}}}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil || res.Output != "finally" || n != 3 {
		t.Fatalf("res=%v err=%v n=%d", res, err, n)
	}
}

func TestFallback(t *testing.T) {
	down := fake.NewFunc(func(model.Request) (model.Response, error) { return model.Response{}, errors.New("down") })
	a := &harness.Agent{Provider: down, Hooks: []harness.Hook{&retry.Retry{Attempts: 2, Base: time.Millisecond,
		Fallback: fake.Script(fake.Text("from fallback"))}}}
	res, err := a.Run(context.Background(), harness.Prompt("x"))
	if err != nil || res.Output != "from fallback" {
		t.Fatalf("res=%v err=%v", res, err)
	}
}
