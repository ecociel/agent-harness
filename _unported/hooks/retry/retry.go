// Package retry makes the model call resilient: transient failures are
// retried with exponential backoff and jitter, and after the last attempt an
// optional fallback provider is tried (ecociel/agents cmd/patterns/resilient).
package retry

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
)

// Retry is the hook.
type Retry struct {
	// Attempts is the total number of tries against the primary provider
	// (default 3).
	Attempts int
	// Base is the first backoff (default 200ms); it doubles per attempt.
	Base time.Duration
	// Retryable classifies errors; nil retries everything except context
	// cancellation and *harness.StopError.
	Retryable func(error) bool
	// Fallback, if set, is tried once after the primary gives up.
	Fallback model.Provider
	// FallbackModel replaces the model name for the fallback call.
	FallbackModel string
}

// Name implements harness.Hook.
func (r *Retry) Name() string { return "retry" }

// WrapModel retries next.
func (r *Retry) WrapModel(run *harness.Run, next harness.ModelCall) harness.ModelCall {
	attempts := max(r.Attempts, 1)
	if r.Attempts == 0 {
		attempts = 3
	}
	base := r.Base
	if base <= 0 {
		base = 200 * time.Millisecond
	}
	retryable := r.Retryable
	if retryable == nil {
		retryable = defaultRetryable
	}
	return func(ctx context.Context, req model.Request) (model.Response, error) {
		var err error
		for i := 0; i < attempts; i++ {
			var resp model.Response
			resp, err = next(ctx, req)
			if err == nil || !retryable(err) || ctx.Err() != nil {
				if err == nil && i > 0 {
					run.Note(r.Name(), "succeeded on attempt %d/%d", i+1, attempts)
				}
				return resp, err
			}
			run.Note(r.Name(), "attempt %d/%d failed: %v", i+1, attempts, err)
			run.Set("retry.attempts", i+1)
			if i == attempts-1 {
				break
			}
			d := base << i
			d += time.Duration(rand.Int64N(int64(d)/2 + 1))
			select {
			case <-ctx.Done():
				return model.Response{}, context.Cause(ctx)
			case <-time.After(d):
			}
		}
		if r.Fallback != nil && retryable(err) {
			freq := req.Clone()
			if r.FallbackModel != "" {
				freq.Model = r.FallbackModel
			}
			run.Set("retry.fallback", true)
			run.Note(r.Name(), "primary gave up after %d attempts; using fallback", attempts)
			return model.Collect(r.Fallback.Complete(ctx, freq), nil)
		}
		return model.Response{}, err
	}
}

func defaultRetryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	_, stop := harness.AsStop(err)
	return !stop
}
