package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ecociel/agent-harness/model"
)

// Hook is anything attached to an Agent. A hook declares which plugin points it
// uses by implementing one or more of the interfaces below; the machine discovers
// them with type assertions. Hooks run in the order they are listed, and the
// first middleware is the outermost layer.
//
// The rule that makes hooks safe under replay: a hook only ever returns a value.
// It gets a Run by value and cannot change it. The shell records what the hook
// returned, and Fold reads the record. Hooks are not on the replay path at all,
// so a decision can never be re-evaluated differently. A hook can run more than
// once if a crash lands before its result is recorded, so its own side effects
// must tolerate repeats. Exactly one result is ever acted on.
type Hook interface {
	Name() string
}

// Point names a plugin point.
type Point string

// The plugin points, in the order a run meets them.
const (
	PointBeforeRun   Point = "BeforeRun"
	PointContext     Point = "Context"
	PointBeforeModel Point = "BeforeModel"
	PointWrapModel   Point = "WrapModel"
	PointAfterModel  Point = "AfterModel"
	PointRecover     Point = "Recover"
	PointBeforeTool  Point = "BeforeTool"
	PointWrapTool    Point = "WrapTool"
	PointAfterTool   Point = "AfterTool"
	PointCheckOutput Point = "CheckOutput"
	PointAfterRun    Point = "AfterRun"
	PointRedact      Point = "Redact"
)

// Points lists every plugin point in run order.
func Points() []Point {
	return []Point{PointBeforeRun, PointContext, PointBeforeModel, PointWrapModel, PointAfterModel,
		PointRecover, PointBeforeTool, PointWrapTool, PointAfterTool, PointCheckOutput, PointAfterRun, PointRedact}
}

// Redactor is the innermost ring. It wraps the provider and the tools directly,
// inside WrapModel and WrapTool, and every durable event passes through it again,
// so no hook, sink or store meets text it has not seen. The model and the tools
// work from what it left, so a tool that needs the original value detokenises in
// its own WrapTool. Kind says where the text is going. A Redactor error fails the
// run with the text discarded. Runs started from a tool inherit it, like the
// Journal. It may be called concurrently.
//
// A Redactor cannot judge a text fragment, so while one is attached a reply reaches
// WrapModel and the sinks only once it is complete. A Redactor that also implements
// ModelMiddleware filters the stream itself, inside the ring, and streaming is live
// again.
type Redactor interface {
	Hook
	Redact(ctx context.Context, kind Kind, text string) (string, error)
}

// BeforeRunHook runs each time a worker takes the run: at the start and on every
// resume. Admission, input guardrails and revocation checks live here; r.Claims,
// r.Drift and r.Waited say what happened since. Return Stop to end the run.
type BeforeRunHook interface {
	Hook
	BeforeRun(ctx context.Context, r Run) error
}

// ContextHook may change the history for good: load memory, compact. It runs
// before every model call, and the Edit it returns is recorded, so a summary is
// computed once. This is the only durable way to change history.
type ContextHook interface {
	Hook
	Context(ctx context.Context, r Run) (Edit, error)
}

// BeforeModelHook edits the request for this one call: retrieval, model profiles,
// hidden tools. Nothing it does is recorded except which tools were offered. A
// call to a tool not offered is denied without running a hook.
type BeforeModelHook interface {
	Hook
	BeforeModel(ctx context.Context, r Run, req *model.Request) error
}

// ModelMiddleware wraps the provider stream: streaming guardrails, stream
// redaction, response caching, wire logging. It sees the stream before the
// collector and the sinks. It may rewrite events, or end the stream with a Stop
// error to halt mid-reply. Retries belong in RecoverHook so they are journaled.
type ModelMiddleware interface {
	Hook
	WrapModel(r Run, next model.Provider) model.Provider
}

// AfterModelHook revises a reply before it is recorded. What it returns is what
// the journal, the history and every later hook get; what it was given is gone. It
// cannot change Usage, because the provider billed what it billed. It sees only
// what the redactors left. Stop ends the run with the reply discarded and its usage
// kept. A crash before the record loses the reply, so the hook runs on the next one.
type AfterModelHook interface {
	Hook
	AfterModel(ctx context.Context, r Run, resp model.Response) (model.Response, error)
}

// RecoverHook decides what follows a failed model call. The first hook that
// retries wins, and the decision is recorded. With no hook, a failure fails the
// run.
type RecoverHook interface {
	Hook
	Recover(ctx context.Context, r Run, f Failure) Recovery
}

// BeforeToolHook decides one call. The first non-Allow decision wins. Decisions
// are recorded, and any call that has not started is decided again after a claim
// or a resolved wait, with the signals so far in c.Signals, so an approval is
// always re-checked against the world it now runs in.
type BeforeToolHook interface {
	Hook
	BeforeTool(ctx context.Context, r Run, c Call) Decision
}

// ToolFunc executes one call and returns the content for the model.
type ToolFunc func(ctx context.Context, c Call) (string, error)

// ToolMiddleware wraps one execution: spans, offloading, a ledger lookup on c.Key.
type ToolMiddleware interface {
	Hook
	WrapTool(r Run, next ToolFunc) ToolFunc
}

// AfterToolHook revises a result before it is recorded. content is the text meant
// for the model, an error's text included. What it returns is what the journal and
// the model get. It does not run for a parked call, a handoff, or a result an
// operator supplied for an unknown outcome. Stop ends the run in the same append
// as the call's record, with the result withheld. Hooks run one at a time, on the
// run's goroutine, in the order calls finish.
type AfterToolHook interface {
	Hook
	AfterTool(ctx context.Context, r Run, c Call, content string) (string, error)
}

// OutputHook judges the candidate answer. Output guardrails and verifiers live
// here. The verdict is recorded.
type OutputHook interface {
	Hook
	CheckOutput(ctx context.Context, r Run, output string) Verdict
}

// AfterRunHook runs when the outcome is decided, before the run is marked ended,
// so it runs at least once even across a crash. Saving memory lives here; key
// repeats on r.ID.
type AfterRunHook interface {
	Hook
	AfterRun(ctx context.Context, r Run)
}

// Note attaches a remark to the current hook invocation's event. It goes through
// the redactors like any other text.
func Note(ctx context.Context, format string, args ...any) {
	if n, ok := ctx.Value(keyNote).(*string); ok {
		*n = fmt.Sprintf(format, args...)
	}
}

// Attribute sets the provenance of a composite's answer, from inside its Body. The
// answer joins the history with this as its Message.Source, so it survives into the
// next run of a session.
func Attribute(ctx context.Context, source string) {
	if s, ok := ctx.Value(keySource).(*string); ok {
		*s = source
	}
}

// Edit replaces Messages[From:To] with With, stamping Source on each new message.
// The zero Edit changes nothing. Inserting is From == To. The result must keep
// every tool call paired with its result, or the run fails. Messages after the edit
// lose their Opaque: provider state can depend on everything before it, as an
// Anthropic signature does, so an edit would otherwise leave it stale.
type Edit struct {
	From   int             `json:"from"`
	To     int             `json:"to"`
	With   []model.Message `json:"with,omitempty"`
	Source string          `json:"source,omitempty"`
}

func (e Edit) zero() bool { return e.From == 0 && e.To == 0 && len(e.With) == 0 }

// Failure describes one failed model call.
type Failure struct {
	Class      model.ErrorClass `json:"class"`
	Detail     string           `json:"detail,omitempty"`
	Attempt    int              `json:"attempt,omitempty"`
	RetryAfter time.Duration    `json:"retry_after_ns,omitempty"`
	Usage      model.Usage      `json:"usage"`
}

// Recovery is what a RecoverHook decided. The zero value gives up.
type Recovery struct {
	Retry bool `json:"retry,omitempty"`
	// After is the backoff, raised to the provider's Retry-After. A backoff over
	// 30 seconds parks a durable run on a WaitTimer instead of sleeping.
	After time.Duration `json:"after_ns,omitempty"`
	// Model replaces the model name until the next reply.
	Model string `json:"model,omitempty"`
}

// The wait kinds the harness opens itself. Every other Kind is the consumer's.
const (
	WaitTimer          = "timer"
	WaitUnknownOutcome = "unknown_outcome"
)

// Wait is one thing a suspended run needs. Hooks and tools fill in Kind, Detail
// and TTL. The harness fills in the rest: ID is unique to this wait, so a signal
// sent for an earlier ask cannot satisfy a later one.
type Wait struct {
	ID      string        `json:"id"`
	Run     string        `json:"run"`  // the run that opened it: a child, for a nested wait
	Call    int           `json:"call"` // batch index, -1 if none
	Kind    string        `json:"kind"` // "approval", "user", WaitTimer, ...
	Detail  string        `json:"detail,omitempty"`
	TTL     time.Duration `json:"-"`                // input only
	Expires time.Time     `json:"expires,omitzero"` // zero means never
}

// Signal resolves a Wait. Value means whatever the wait's owner says it means.
type Signal struct {
	Wait    string `json:"wait"`
	Value   string `json:"value,omitempty"`
	By      string `json:"by,omitempty"`
	Kind    string `json:"kind,omitempty"`    // the wait's Kind; the harness fills it in
	Expired bool   `json:"expired,omitempty"` // no one answered in time
	// Rerun answers a WaitUnknownOutcome: whoever answered knows the call did not
	// take effect, so it is decided again and runs again under the same key, as
	// attempt n+1, instead of taking Value as its result. Other waits ignore it.
	Rerun bool `json:"rerun,omitempty"`
}

type suspension struct{ waits []Wait }

func (s *suspension) Error() string { return fmt.Sprintf("suspended on %d waits", len(s.waits)) }

// Suspend is the error a tool returns to park its call. The harness invokes the
// tool again when the waits resolve, with the answers in Signals(ctx).
func Suspend(waits ...Wait) error { return &suspension{waits: waits} }

// Action is the outcome of a BeforeTool decision.
type Action string

// Decision actions. Stored in journals, so the values are part of the schema.
const (
	ActionAllow Action = ""
	ActionDeny  Action = "deny"
	ActionHalt  Action = "halt"
	ActionAwait Action = "await"
)

// Decision is a BeforeTool verdict.
type Decision struct {
	Action Action     `json:"action,omitempty"`
	Reason string     `json:"reason,omitempty"`
	Stop   StopReason `json:"stop,omitempty"`
	Wait   *Wait      `json:"-"` // moved onto Event.Waits when recorded
	// Tags are facts about the decision that a later reader of the journal needs
	// without the hook: a risk class, a reason code. They are recorded on tool.gated,
	// their values redacted like any text, and never shown to the model.
	Tags map[string]string `json:"tags,omitempty"`
}

// Allow returns a Decision that lets the call run.
func Allow() Decision { return Decision{} }

// Deny returns a Decision that skips the call and feeds reason back.
func Deny(reason string) Decision { return Decision{Action: ActionDeny, Reason: reason} }

// Halt returns a Decision that stops the run with StopGuardrail.
func Halt(reason string) Decision {
	return Decision{Action: ActionHalt, Reason: reason, Stop: StopGuardrail}
}

// Await returns a Decision that suspends the run until w resolves.
func Await(w Wait) Decision { return Decision{Action: ActionAwait, Wait: &w} }

func (d Decision) outcome() string {
	if d.Action == ActionAllow {
		return "allow"
	}
	return string(d.Action)
}

// VerdictAction is the outcome of an output check.
type VerdictAction string

// Verdict actions. Stored in journals, so the values are part of the schema.
const (
	VerdictAccept VerdictAction = ""
	VerdictRevise VerdictAction = "revise"
	VerdictBlock  VerdictAction = "block"
)

// Verdict is a CheckOutput result.
type Verdict struct {
	Action   VerdictAction `json:"action,omitempty"`
	Feedback string        `json:"feedback,omitempty"`
	// Rewrite, if non-empty on Accept, replaces the answer.
	Rewrite string `json:"rewrite,omitempty"`
}

// Accept returns an accepting Verdict.
func Accept() Verdict { return Verdict{} }

// Revise returns a Verdict that asks the model to try again.
func Revise(feedback string) Verdict { return Verdict{Action: VerdictRevise, Feedback: feedback} }

// Block returns a Verdict that stops the run.
func Block(reason string) Verdict { return Verdict{Action: VerdictBlock, Feedback: reason} }

// StopReason says why a run ended, or why Run returned before it did.
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
	StopRefused    StopReason = "refused"   // the provider refused or filtered the reply
	StopTruncated  StopReason = "truncated" // the answer hit the output limit
	StopSuspended  StopReason = "suspended" // not over: see Run.Waits
)

// StopError is the error a hook returns to end a run on purpose. The run ends in
// StateStopped with Reason; it is not reported as a failure.
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
	redact      []Redactor
	beforeRun   []BeforeRunHook
	context     []ContextHook
	beforeModel []BeforeModelHook
	wrapModel   []ModelMiddleware
	afterModel  []AfterModelHook
	recover     []RecoverHook
	beforeTool  []BeforeToolHook
	wrapTool    []ToolMiddleware
	afterTool   []AfterToolHook
	output      []OutputHook
	afterRun    []AfterRunHook
}

func collect[T any](hs []Hook) []T {
	var out []T
	for _, h := range hs {
		if x, ok := h.(T); ok {
			out = append(out, x)
		}
	}
	return out
}

// outsideRing drops the middleware that belongs inside the redaction ring.
func outsideRing(ms []ModelMiddleware) []ModelMiddleware {
	return slices.DeleteFunc(ms, func(m ModelMiddleware) bool {
		_, ok := m.(Redactor)
		return ok
	})
}

func indexHooks(hs []Hook) hookSet {
	return hookSet{
		redact:      collect[Redactor](hs),
		beforeRun:   collect[BeforeRunHook](hs),
		context:     collect[ContextHook](hs),
		beforeModel: collect[BeforeModelHook](hs),
		wrapModel:   outsideRing(collect[ModelMiddleware](hs)),
		afterModel:  collect[AfterModelHook](hs),
		recover:     collect[RecoverHook](hs),
		beforeTool:  collect[BeforeToolHook](hs),
		wrapTool:    collect[ToolMiddleware](hs),
		afterTool:   collect[AfterToolHook](hs),
		output:      collect[OutputHook](hs),
		afterRun:    collect[AfterRunHook](hs),
	}
}

// HookPoints returns the points h implements, in run order.
func HookPoints(h Hook) []Point {
	s := indexHooks([]Hook{h})
	has := map[Point]bool{
		PointBeforeRun: len(s.beforeRun) > 0, PointContext: len(s.context) > 0,
		PointBeforeModel: len(s.beforeModel) > 0, PointWrapModel: len(s.wrapModel) > 0,
		PointAfterModel: len(s.afterModel) > 0, PointRecover: len(s.recover) > 0,
		PointBeforeTool: len(s.beforeTool) > 0, PointWrapTool: len(s.wrapTool) > 0,
		PointAfterTool: len(s.afterTool) > 0, PointCheckOutput: len(s.output) > 0,
		PointAfterRun: len(s.afterRun) > 0, PointRedact: len(s.redact) > 0,
	}
	var out []Point
	for _, p := range Points() {
		if has[p] {
			out = append(out, p)
		}
	}
	return out
}
