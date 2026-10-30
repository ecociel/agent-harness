package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/tool"
)

// Runner is anything that turns an Input into a finished or paused Run: a single Agent, or a
// multi-agent pattern from package multi. Because patterns are Runners too, they
// compose: a Router can route to a Parallel of Pipelines of Agents.
type Runner interface {
	Run(ctx context.Context, in Input) (*Run, error)
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(ctx context.Context, in Input) (*Run, error)

// Run calls f.
func (f RunnerFunc) Run(ctx context.Context, in Input) (*Run, error) { return f(ctx, in) }

// Named is implemented by Runners that have a display name.
type Named interface{ RunnerName() string }

// NameOf returns r's display name, or its Go type.
func NameOf(r Runner) string {
	if n, ok := r.(Named); ok {
		return n.RunnerName()
	}
	return fmt.Sprintf("%T", r)
}

// Input says which run to advance and with what. Run is idempotent on RunID:
// an unknown ID starts a run, a known one continues it from its journal, and a
// finished one is returned without doing anything. The same call from any
// process, any number of times, converges on the same run.
type Input struct {
	// RunID names the run. Empty generates one, which is only useful for a run
	// nobody will need to resume.
	RunID string
	// Messages starts the run. They are ignored once the run exists.
	Messages []model.Message
	// Signals resolve open waits. A repeat of a delivered signal is ignored.
	Signals []Signal
	// Cancel, if set, ends the run with StopCancelled from any process. A worker
	// still running it is fenced. This is the way to end a durable run on purpose:
	// a cancelled context only stops the worker.
	Cancel string
}

// Prompt returns an Input holding one user message.
func Prompt(text string) Input { return Input{Messages: []model.Message{model.User(text)}} }

// Resume returns the Input that continues a run, delivering signals if any.
func Resume(runID string, signals ...Signal) Input {
	return Input{RunID: runID, Signals: signals}
}

// Text returns the last user message of in.
func (in Input) Text() string { return model.LastUser(in.Messages) }

// Agent is a single model-plus-tools loop. Provider is required. An Agent is safe
// for concurrent use as long as its hooks are.
type Agent struct {
	// Name identifies the agent in events, traces and patterns.
	Name string
	// Instructions is the system prompt. Pinned when a run starts.
	Instructions string
	// Provider is the model backend. Required.
	Provider model.Provider
	// Model is passed through to the provider. Pinned when a run starts.
	Model string
	// Tools the model may call. May be nil.
	Tools *tool.Registry
	// Hooks attached to every run, in order.
	Hooks []Hook
	// Journal makes runs durable. Nil inherits the journal of the run that
	// started this one, and otherwise keeps the run in memory for this call.
	Journal Journal
	// Lease is how long this worker may stay silent before a sweep may treat it as
	// dead. Set it above the slowest step: a sweep that fires early costs a needless
	// unknown-outcome wait, never a repeated effect. Default DefaultLease.
	Lease time.Duration
	// MaxTurns caps model turns. Default 8. Pinned when a run starts.
	MaxTurns int
	// MaxRepairs caps Verify to Compose repair cycles. Default 2. Pinned.
	MaxRepairs int
	// MaxTokens is the per-call output cap (0 = provider default). Pinned.
	MaxTokens int
	// Temperature is passed through to the provider when non-nil. Pinned.
	Temperature *float64
}

// RunnerName implements Named.
func (a *Agent) RunnerName() string { return a.Name }

func newRunID(name string) string {
	if name == "" {
		name = "agent"
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	return name + "-" + hex.EncodeToString(b[:])
}
