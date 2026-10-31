package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
	// Cancel, if set, ends the run and the tree below it with StopCancelled, from any
	// process. This is the way to end a durable run on purpose: a cancelled context
	// only stops the worker.
	//
	// A worker still running the run is fenced. A branch below stops before its next
	// model call or tool, wherever it runs, because it reads its ancestors after
	// recording each intent. A call already in flight finishes, and its result is not
	// recorded. A child with no worker, parked or crashed, is ended through its parent,
	// so that takes a pattern's parts or a ChildTool. Any other child stays where it is,
	// and the same check stops it if a worker ever takes it.
	//
	// ErrFenced means the cancel lost a race with the run's worker: send it again. So
	// does a crash, and a repeat is safe.
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

	composite *Composite // set by Composite.Run: the machine runs its Body
}

// RunnerName implements Named.
func (a *Agent) RunnerName() string { return a.Name }

// Composite is a run whose work is to run others: every multi-agent pattern is one.
// It has no model and no tools, and it is a run like any other. Its input survives in
// the journal, its waits are recorded and refuse a stale signal, a finished one
// answers from its journal, and a sweep finds it. It writes no model.* or tool.*
// event, so a sum of spend over a tree needs no exceptions.
type Composite struct {
	// Name identifies the run in events, traces and patterns.
	Name string
	// Shape says what Body runs, in order. It is pinned when the run starts, and a
	// resume that finds it changed fails the run.
	Shape string
	// Body does the work and keeps no state: it runs again after a crash or an
	// answered wait, so it must reach its children by the same IDs and let the
	// finished ones answer from their journals. in holds this run's ID, the input from
	// the journal, and the answers to the waits it last parked on. It returns the
	// output and what its children have spent so far. Returning Suspend parks the run
	// on the children's waits. Stop ends it for that reason and keeps the output.
	// ErrDrained or ErrFenced stop this worker, with the run unharmed.
	Body func(ctx context.Context, in Input) (output string, below Spend, err error)
	// Cancel ends the children of a cancelled run, and may be nil. It runs at least
	// once, before the run is marked ended, and must tolerate children that finished
	// or never started. It has the same context as Body, so children inherit from it.
	Cancel func(ctx context.Context, id, reason string) error
	// Hooks attach to this run. Redact, BeforeRun, CheckOutput and AfterRun apply, and
	// a CheckOutput that revises ends the run with StopMaxRepairs, because no model is
	// here to try again. A Redactor covers every child.
	Hooks []Hook
	// Journal makes the run durable. Nil inherits, like Agent.Journal.
	Journal Journal
	// Lease is the same as Agent.Lease.
	Lease time.Duration
}

// RunnerName implements Named.
func (c *Composite) RunnerName() string { return c.Name }

// ChildTool is a tool whose calls start runs. A cancelled run calls CancelChild for
// each call it started, with that call's idempotency key on ctx, because it cannot
// tell a child from a tool's other work and only the tool holds the child's Runner.
// CancelChild runs at least once and must tolerate a child that finished or never
// started.
type ChildTool interface {
	tool.Tool
	CancelChild(ctx context.Context, reason string) error
}

// Run implements Runner, with the semantics of Agent.Run.
func (c *Composite) Run(ctx context.Context, in Input) (*Run, error) {
	if c.Body == nil {
		return nil, errors.New("harness: composite has no Body")
	}
	a := &Agent{Name: c.Name, Hooks: c.Hooks, Journal: c.Journal, Lease: c.Lease, composite: c}
	return a.Run(ctx, in)
}

func newRunID(name string) string {
	if name == "" {
		name = "agent"
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	return name + "-" + hex.EncodeToString(b[:])
}
