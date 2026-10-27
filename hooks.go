package harness

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ecociel/agent-harness/model"
)

// Hook is anything attached to an Agent. A hook declares which plugin points
// it uses by implementing one or more of the *Hook / *Middleware interfaces
// below; the machine discovers them with type assertions when the run starts.
// A single value may implement many points (the budget hook, for example,
// uses BeforeModel, AfterModel and AfterTurn).
//
// Hooks run in the order they are listed on the Agent. Middleware wraps in
// the same order: the first ModelMiddleware is the outermost layer.
//
// Name identifies the hook in events, traces and the diagnostic view.
type Hook interface {
	Name() string
}

// Point names a plugin point. Every hook invocation is emitted as an Event
// carrying its Point, so the full life of a run is observable.
type Point string

// The plugin points, in the order a run meets them.
const (
	PointBeforeRun    Point = "BeforeRun"
	PointBeforeModel  Point = "BeforeModel"
	PointWrapModel    Point = "WrapModel"
	PointOnDelta      Point = "OnDelta"
	PointAfterModel   Point = "AfterModel"
	PointBeforeTool   Point = "BeforeTool"
	PointWrapTool     Point = "WrapTool"
	PointAfterTool    Point = "AfterTool"
	PointAfterTurn    Point = "AfterTurn"
	PointCheckOutput  Point = "CheckOutput"
	PointAfterRun     Point = "AfterRun"
	PointOnTransition Point = "OnTransition"
)

// Points lists every plugin point in run order.
func Points() []Point {
	return []Point{PointBeforeRun, PointBeforeModel, PointWrapModel, PointOnDelta,
		PointAfterModel, PointBeforeTool, PointWrapTool, PointAfterTool,
		PointAfterTurn, PointCheckOutput, PointAfterRun, PointOnTransition}
}

// BeforeRunHook runs once in StatePrepare, before the first model call.
// Input guardrails, admission control and run spans live here. Returning a
// *StopError (see Stop) ends the run in StateStopped; any other error fails
// it.
type BeforeRunHook interface {
	Hook
	BeforeRun(ctx context.Context, r *Run) error
}

// BeforeModelHook runs in StateCompose on the request about to be sent. It
// may edit req (add context, swap tools, trim messages). To make a change
// persistent across turns (compaction), also edit r.Messages.
type BeforeModelHook interface {
	Hook
	BeforeModel(ctx context.Context, r *Run, req *model.Request) error
}

// ModelCall performs one model call and returns the accumulated response.
type ModelCall func(ctx context.Context, req model.Request) (model.Response, error)

// ModelMiddleware wraps the model call: retries, fallbacks, caching, spans,
// latency metrics. It must call next at most once per attempt and return its
// result (possibly altered).
type ModelMiddleware interface {
	Hook
	WrapModel(r *Run, next ModelCall) ModelCall
}

// DeltaHook sees every streamed event of a model reply as it arrives. Use it
// for live UIs and streaming guardrails; it must be fast and must not block.
type DeltaHook interface {
	Hook
	OnDelta(ctx context.Context, r *Run, ev model.Event)
}

// AfterModelHook runs on the accumulated response before it is recorded.
// Budget charging and token metrics live here. It may edit resp.
type AfterModelHook interface {
	Hook
	AfterModel(ctx context.Context, r *Run, resp *model.Response) error
}

// BeforeToolHook decides whether one tool call may run: guardrail veto,
// scope checks, human approval. The first non-Allow decision wins.
type BeforeToolHook interface {
	Hook
	BeforeTool(ctx context.Context, r *Run, call model.ToolCall) Decision
}

// ToolCallFunc executes one tool call and returns the content for the model.
type ToolCallFunc func(ctx context.Context, call model.ToolCall) (string, error)

// ToolMiddleware wraps one tool execution: spans, timeouts, result
// offloading, idempotency. Values put into ctx reach the tool (and any
// sub-agent it starts), which is how traces nest across agents.
type ToolMiddleware interface {
	Hook
	WrapTool(r *Run, next ToolCallFunc) ToolCallFunc
}

// AfterToolHook sees each finished (or denied) tool call.
type AfterToolHook interface {
	Hook
	AfterTool(ctx context.Context, r *Run, out ToolOutcome) error
}

// AfterTurnHook runs at the turn boundary in StateObserve, after all tool
// results are recorded. Loop detection and checkpoints live here.
type AfterTurnHook interface {
	Hook
	AfterTurn(ctx context.Context, r *Run) error
}

// OutputHook judges the candidate final answer in StateVerify. Output
// guardrails, schema checks and verifiers live here.
type OutputHook interface {
	Hook
	CheckOutput(ctx context.Context, r *Run, output string) Verdict
}

// AfterRunHook runs once when the run reaches a terminal state, whatever the
// outcome. It must not fail; record problems instead.
type AfterRunHook interface {
	Hook
	AfterRun(ctx context.Context, r *Run, res *Result)
}

// TransitionHook sees every state change. Use it for state-level spans or
// metrics; the same information is also emitted as KindTransition events.
type TransitionHook interface {
	Hook
	OnTransition(ctx context.Context, r *Run, from, to State)
}

// ToolOutcome describes one tool call after the fact.
type ToolOutcome struct {
	Call     model.ToolCall
	Content  string
	Err      error
	Denied   bool
	Reason   string
	Duration time.Duration
}

// Action is the outcome of a BeforeTool decision.
type Action int

// Decision actions.
const (
	// ActionAllow lets the call run.
	ActionAllow Action = iota
	// ActionDeny skips the call and tells the model why, so it can adapt.
	ActionDeny
	// ActionHalt skips the call and stops the whole run.
	ActionHalt
)

// Decision is a BeforeTool verdict.
type Decision struct {
	Action Action
	Reason string
	// Stop is the StopReason used when Action is ActionHalt (default
	// StopGuardrail).
	Stop StopReason
}

// Allow returns a Decision that lets the call run.
func Allow() Decision { return Decision{} }

// Deny returns a Decision that skips the call and feeds reason back.
func Deny(reason string) Decision { return Decision{Action: ActionDeny, Reason: reason} }

// Halt returns a Decision that stops the run with StopGuardrail.
func Halt(reason string) Decision {
	return Decision{Action: ActionHalt, Reason: reason, Stop: StopGuardrail}
}

func (d Decision) outcome() string {
	switch d.Action {
	case ActionDeny:
		return "deny"
	case ActionHalt:
		return "halt"
	}
	return "allow"
}

// VerdictAction is the outcome of an output check.
type VerdictAction int

// Verdict actions.
const (
	// VerdictAccept accepts the answer (optionally rewritten).
	VerdictAccept VerdictAction = iota
	// VerdictRevise rejects the answer and sends Feedback back to the model
	// for another attempt (bounded by Agent.MaxRepairs).
	VerdictRevise
	// VerdictBlock rejects the answer and stops the run with StopGuardrail.
	VerdictBlock
)

// Verdict is a CheckOutput result.
type Verdict struct {
	Action   VerdictAction
	Feedback string
	// Rewrite, if non-empty on Accept, replaces the answer (redaction).
	Rewrite string
}

// Accept returns an accepting Verdict.
func Accept() Verdict { return Verdict{} }

// Revise returns a Verdict that asks the model to try again.
func Revise(feedback string) Verdict { return Verdict{Action: VerdictRevise, Feedback: feedback} }

// Block returns a Verdict that stops the run.
func Block(reason string) Verdict { return Verdict{Action: VerdictBlock, Feedback: reason} }

func (v Verdict) outcome() string {
	switch v.Action {
	case VerdictRevise:
		return "revise"
	case VerdictBlock:
		return "block"
	}
	if v.Rewrite != "" {
		return "rewrite"
	}
	return "accept"
}

// StopReason says why a run ended.
type StopReason string

// Stop reasons.
const (
	StopCompleted  StopReason = "completed"
	StopMaxTurns   StopReason = "max_turns"
	StopMaxRepairs StopReason = "max_repairs"
	StopBudget     StopReason = "budget_exhausted"
	StopGuardrail  StopReason = "guardrail"
	StopPolicy     StopReason = "policy"
	StopLoop       StopReason = "loop_detected"
	StopHandoff    StopReason = "handoff"
	StopCancelled  StopReason = "cancelled"
	StopFailed     StopReason = "failed"
)

// StopError is the error a hook returns to end a run on purpose. The run
// ends in StateStopped with Reason; it is not reported as a failure.
type StopError struct {
	Reason StopReason
	Detail string
}

func (e *StopError) Error() string {
	if e.Detail == "" {
		return "stop: " + string(e.Reason)
	}
	return fmt.Sprintf("stop: %s: %s", e.Reason, e.Detail)
}

// Stop returns a *StopError.
func Stop(reason StopReason, detail string) error {
	return &StopError{Reason: reason, Detail: detail}
}

// AsStop reports whether err is (or wraps) a *StopError.
func AsStop(err error) (*StopError, bool) {
	var s *StopError
	ok := errors.As(err, &s)
	return s, ok
}

// hookSet is the per-run index of hooks by point.
type hookSet struct {
	beforeRun   []BeforeRunHook
	beforeModel []BeforeModelHook
	wrapModel   []ModelMiddleware
	onDelta     []DeltaHook
	afterModel  []AfterModelHook
	beforeTool  []BeforeToolHook
	wrapTool    []ToolMiddleware
	afterTool   []AfterToolHook
	afterTurn   []AfterTurnHook
	output      []OutputHook
	afterRun    []AfterRunHook
	transition  []TransitionHook
}

func indexHooks(hs []Hook) hookSet {
	var s hookSet
	for _, h := range hs {
		if x, ok := h.(BeforeRunHook); ok {
			s.beforeRun = append(s.beforeRun, x)
		}
		if x, ok := h.(BeforeModelHook); ok {
			s.beforeModel = append(s.beforeModel, x)
		}
		if x, ok := h.(ModelMiddleware); ok {
			s.wrapModel = append(s.wrapModel, x)
		}
		if x, ok := h.(DeltaHook); ok {
			s.onDelta = append(s.onDelta, x)
		}
		if x, ok := h.(AfterModelHook); ok {
			s.afterModel = append(s.afterModel, x)
		}
		if x, ok := h.(BeforeToolHook); ok {
			s.beforeTool = append(s.beforeTool, x)
		}
		if x, ok := h.(ToolMiddleware); ok {
			s.wrapTool = append(s.wrapTool, x)
		}
		if x, ok := h.(AfterToolHook); ok {
			s.afterTool = append(s.afterTool, x)
		}
		if x, ok := h.(AfterTurnHook); ok {
			s.afterTurn = append(s.afterTurn, x)
		}
		if x, ok := h.(OutputHook); ok {
			s.output = append(s.output, x)
		}
		if x, ok := h.(AfterRunHook); ok {
			s.afterRun = append(s.afterRun, x)
		}
		if x, ok := h.(TransitionHook); ok {
			s.transition = append(s.transition, x)
		}
	}
	return s
}

// HookPoints returns the points h implements, in run order. The diagnostic
// view uses it to draw which hook sits where.
func HookPoints(h Hook) []Point {
	s := indexHooks([]Hook{h})
	var out []Point
	add := func(n int, p Point) {
		if n > 0 {
			out = append(out, p)
		}
	}
	add(len(s.beforeRun), PointBeforeRun)
	add(len(s.beforeModel), PointBeforeModel)
	add(len(s.wrapModel), PointWrapModel)
	add(len(s.onDelta), PointOnDelta)
	add(len(s.afterModel), PointAfterModel)
	add(len(s.beforeTool), PointBeforeTool)
	add(len(s.wrapTool), PointWrapTool)
	add(len(s.afterTool), PointAfterTool)
	add(len(s.afterTurn), PointAfterTurn)
	add(len(s.output), PointCheckOutput)
	add(len(s.afterRun), PointAfterRun)
	add(len(s.transition), PointOnTransition)
	return out
}
