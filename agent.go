package harness

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/tool"
)

// Runner is anything that turns an Input into a Result: a single Agent, or a
// multi-agent pattern from package multi. Because patterns are Runners too,
// they compose: a Router can route to a Parallel of Pipelines of Agents.
type Runner interface {
	Run(ctx context.Context, in Input) (*Result, error)
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(ctx context.Context, in Input) (*Result, error)

// Run calls f.
func (f RunnerFunc) Run(ctx context.Context, in Input) (*Result, error) { return f(ctx, in) }

// Named is implemented by Runners that have a display name.
type Named interface{ RunnerName() string }

// NameOf returns r's display name, or its Go type.
func NameOf(r Runner) string {
	if n, ok := r.(Named); ok {
		return n.RunnerName()
	}
	return fmt.Sprintf("%T", r)
}

// Input is what a run starts from: the conversation so far (usually ending in
// a user message). The agent's Instructions are added as a system message at
// request time and are not part of Input.
type Input struct {
	Messages []model.Message
}

// Prompt returns an Input holding one user message.
func Prompt(text string) Input { return Input{Messages: []model.Message{model.User(text)}} }

// Text returns the last user message of in.
func (in Input) Text() string { return model.LastUser(in.Messages) }

// Agent is a single model-plus-tools loop driven by the state machine. The
// zero value is not usable: Provider is required. An Agent is safe for
// concurrent use as long as its hooks are.
type Agent struct {
	// Name identifies the agent in events, traces and patterns.
	Name string
	// Instructions is the system prompt.
	Instructions string
	// Provider is the model backend. Required.
	Provider model.Provider
	// Model is passed through to the provider.
	Model string
	// Tools the model may call. May be nil.
	Tools *tool.Registry
	// Hooks attached to every run, in order.
	Hooks []Hook
	// MaxTurns caps inner-loop iterations (model calls). Default 8.
	MaxTurns int
	// MaxRepairs caps Verify → Compose repair cycles. Default 2.
	MaxRepairs int
	// MaxTokens is the per-call output cap passed to the provider (0 = provider default).
	MaxTokens int
	// Temperature is passed through to the provider when non-nil.
	Temperature *float64
}

// RunnerName implements Named.
func (a *Agent) RunnerName() string { return a.Name }

// Result is the outcome of a run.
type Result struct {
	RunID  string `json:"run_id"`
	Agent  string `json:"agent"`
	Output string `json:"output"`
	// Messages is the full conversation after the run (without the system
	// instructions), ready to feed into the next run of a session.
	Messages []model.Message `json:"messages"`
	State    State           `json:"state"`
	Stop     StopReason      `json:"stop"`
	Detail   string          `json:"detail,omitempty"`
	Usage    model.Usage     `json:"usage"`
	Turns    int             `json:"turns"`
	Repairs  int             `json:"repairs"`
	// ModelCalls and ToolCalls count calls made by this run only; sub-agent
	// usage is in their own Results (and in Usage when a pattern sums it).
	ModelCalls int           `json:"model_calls"`
	ToolCalls  int           `json:"tool_calls"`
	Duration   time.Duration `json:"duration_ns"`
	// Path is the sequence of states visited, Start first.
	Path []State `json:"path"`
	// Handoff is set when the run ended because a tool transferred control.
	Handoff *tool.Handoff `json:"handoff,omitempty"`
}

// OK reports whether the run completed with an accepted answer.
func (r *Result) OK() bool { return r != nil && r.Stop == StopCompleted }

// Run is the live state of one run, passed to every hook. Hooks run on the
// run's goroutine one at a time, so they may read and write the exported
// fields without locking. Set/Value are safe from any goroutine.
type Run struct {
	ID        string
	ParentRun string
	Agent     *Agent
	Depth     int
	State     State
	Turn      int
	Repairs   int
	// Messages is the conversation history (no system instructions). Hooks
	// may rewrite it, e.g. to compact.
	Messages   []model.Message
	Usage      model.Usage
	ModelCalls int
	ToolCalls  int
	Started    time.Time
	// Last is the most recent model response.
	Last model.Response

	mu     sync.Mutex
	values map[string]any
	notes  map[string]string
}

// Note attaches a human-readable remark to the current invocation of the
// named hook, e.g. "compacted 12 messages". The machine puts it on the hook
// Event it emits for that invocation (Event.Text), so diagnostics and logs
// show what a hook actually did, not just that it ran. Calling Note again
// before the event is emitted replaces the remark.
func (r *Run) Note(hook, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.notes == nil {
		r.notes = map[string]string{}
	}
	r.notes[hook] = fmt.Sprintf(format, args...)
}

// takeNote returns and clears the remark for hook.
func (r *Run) takeNote(hook string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.notes[hook]
	delete(r.notes, hook)
	return n
}

// Set stores a per-run value for hooks (scratch space shared by points).
func (r *Run) Set(key string, v any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values == nil {
		r.values = map[string]any{}
	}
	r.values[key] = v
}

// Value returns a value stored with Set.
func (r *Run) Value(key string) any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.values[key]
}

var runSeq atomic.Int64

func newRunID(name string) string {
	if name == "" {
		name = "agent"
	}
	return fmt.Sprintf("%s-%d", name, runSeq.Add(1))
}
