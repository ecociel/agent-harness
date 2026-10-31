// Package retry decides what follows a failed model call: try again with backoff, and
// after the last attempt, optionally move to another model.
//
// It is a RecoverHook, so every attempt and every delay is a recorded decision. A
// crash in the middle of a backoff loses nothing, Retry-After is honoured by the
// machine, and a wait too long to sleep through parks a durable run instead of holding
// a worker.
//
// A fallback on another provider has to sit inside the redaction ring, which wraps
// Agent.Provider, so it is a provider and not a hook:
//
//	agent.Provider = retry.Fallback(primary, backup, "backup-model")
//	agent.Hooks = []harness.Hook{&retry.Retry{FallbackModel: "backup-model"}}
package retry

import (
	"context"
	"iter"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
)

// Retry is the hook.
type Retry struct {
	// Attempts is the total number of tries before giving up (default 3).
	Attempts int
	// Base is the first backoff (default 200ms). It doubles per attempt, with jitter.
	Base time.Duration
	// Max caps the backoff before jitter (default 20s).
	Max time.Duration
	// Retryable decides by failure. Nil retries the classes another try can fix.
	Retryable func(harness.Failure) bool
	// FallbackModel, if set, gets one try after the last attempt.
	FallbackModel string
}

// Name implements harness.Hook.
func (h *Retry) Name() string { return "retry" }

// Recover implements harness.RecoverHook.
func (h *Retry) Recover(_ context.Context, _ harness.Run, f harness.Failure) harness.Recovery {
	if retryable := h.Retryable; retryable != nil && !retryable(f) || retryable == nil && !defaultRetryable(f) {
		return harness.Recovery{}
	}
	attempts := h.Attempts
	if attempts == 0 {
		attempts = 3
	}
	switch {
	case f.Attempt < attempts:
		return harness.Recovery{Retry: true, After: h.backoff(f.Attempt)}
	case f.Attempt == attempts && h.FallbackModel != "":
		return harness.Recovery{Retry: true, Model: h.FallbackModel}
	}
	return harness.Recovery{}
}

func (h *Retry) backoff(attempt int) time.Duration {
	base, ceiling := h.Base, h.Max
	if base <= 0 {
		base = 200 * time.Millisecond
	}
	if ceiling <= 0 {
		ceiling = 20 * time.Second
	}
	d := min(base<<min(attempt-1, 20), ceiling)
	return d + time.Duration(rand.Int64N(int64(d)/2+1))
}

// A context_length failure is the compact hook's to answer, and the rest cannot be
// fixed by asking again.
func defaultRetryable(f harness.Failure) bool {
	return slices.Contains([]model.ErrorClass{model.ClassTransient, model.ClassRateLimit, model.ClassUnknown}, f.Class)
}

// Fallback returns a provider that sends calls for name to fallback and the rest to
// primary. The model name is recorded on each attempt, so a resume picks the same
// provider.
func Fallback(primary, fallback model.Provider, name string) model.Provider {
	return model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
		if req.Model == name {
			return fallback.Complete(ctx, req)
		}
		return primary.Complete(ctx, req)
	})
}
