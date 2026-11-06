package harness

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/tool"
)

// Kind classifies an Event.
type Kind string

// Durable kinds. These are the journal: Seq > 0, recorded before anything acts
// on them, and everything about a run is a fold over them.
const (
	KindRunStarted    Kind = "run.started"    // identity, Pin, input
	KindRunClaimed    Kind = "run.claimed"    // a worker took the run; fences the previous one
	KindHistoryEdited Kind = "history.edited" // a Context hook's durable edit
	KindBodyDone      Kind = "body.done"      // how a composite run's work came out
	KindModelStarted  Kind = "model.started"  // intent: a paid call is about to happen
	KindModelDone     Kind = "model.done"     // the reply and its usage
	KindModelFailed   Kind = "model.failed"   // classified failure and the recovery decided
	KindModelLost     Kind = "model.lost"     // a started call has no outcome: billing unknown
	KindToolGated     Kind = "tool.gated"     // a BeforeTool decision
	KindToolStarted   Kind = "tool.started"   // intent: the effect is about to happen
	KindToolDone      Kind = "tool.done"      // the result for the model
	KindToolLost      Kind = "tool.lost"      // a started call has no outcome
	KindWaitResolved  Kind = "wait.resolved"  // a signal arrived, or the wait expired
	KindOutputChecked Kind = "output.checked" // the CheckOutput verdict
	KindRunStopping   Kind = "run.stopping"   // the run will end for this reason
	KindRunEnded      Kind = "run.ended"      // AfterRun hooks ran
)

// Ephemeral kinds. Seq is 0. They go to sinks only and nothing depends on them.
const (
	KindDelta   Kind = "delta"   // one streamed text chunk
	KindHook    Kind = "hook"    // one hook invocation: Point, Outcome, Duration
	KindPattern Kind = "pattern" // a multi-agent pattern step
)

// Event is the one record of what happened. The journal stores events, sinks
// receive the same events, and a run is a fold over them, so there is no second
// vocabulary to keep in step. Kind says which payload fields are set. Only the
// harness writes durable events.
type Event struct {
	Seq       int64     `json:"seq,omitempty"`
	Time      time.Time `json:"time"`
	Kind      Kind      `json:"kind"`
	RunID     string    `json:"run_id,omitempty"`
	RootRun   string    `json:"root_run,omitempty"`
	ParentRun string    `json:"parent_run,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	Depth     int       `json:"depth"`
	Turn      int       `json:"turn,omitempty"`
	From      State     `json:"from,omitempty"`
	To        State     `json:"to,omitempty"`
	// Wake is set on the last event of an append. See Journal.
	Wake time.Time `json:"wake,omitzero"`

	Hook     string        `json:"hook,omitempty"` // the hook that decided
	Tool     string        `json:"tool,omitempty"` // tool.*
	Point    Point         `json:"point,omitempty"`
	Model    string        `json:"model,omitempty"` // model.started, model.done, model.failed: empty means the provider's default
	Outcome  string        `json:"outcome,omitempty"`
	Text     string        `json:"text,omitempty"`
	Duration time.Duration `json:"duration_ns,omitempty"`
	Index    int           `json:"index,omitempty"`   // tool.*: the call's position in the batch
	Attempt  int           `json:"attempt,omitempty"` // model.*, tool.started

	Pin      *Pin              `json:"pin,omitempty"`      // run.started
	Messages []model.Message   `json:"messages,omitempty"` // run.started
	After    string            `json:"after,omitempty"`    // run.started
	Pinned   []model.Message   `json:"pinned,omitempty"`   // run.started
	Meta     map[string]string `json:"meta,omitempty"`     // run.started
	Drift    *Drift            `json:"drift,omitempty"`    // run.claimed
	Edit     *Edit             `json:"edit,omitempty"`     // history.edited
	Offered  []string          `json:"offered,omitempty"`  // model.started
	Reply    *model.Response   `json:"reply,omitempty"`    // model.done, and a body's answer on body.done
	Below    *Spend            `json:"below,omitempty"`    // body.done
	Failure  *Failure          `json:"failure,omitempty"`  // model.failed, run.stopping
	Recovery *Recovery         `json:"recovery,omitempty"` // model.failed
	Decision *Decision         `json:"decision,omitempty"` // tool.gated
	Effect   tool.Effect       `json:"effect,omitempty"`   // tool.gated
	Handoff  *tool.Handoff     `json:"handoff,omitempty"`  // tool.done
	Waits    []Wait            `json:"waits,omitempty"`    // the waits this event opens
	Signal   *Signal           `json:"signal,omitempty"`   // wait.resolved
	Verdict  *Verdict          `json:"verdict,omitempty"`  // output.checked
	Stop     StopReason        `json:"stop,omitempty"`     // run.stopping, body.done
}

// EventSink receives events. It is called synchronously from the run (and, for
// parallel tools or patterns, from several goroutines), so it must be safe for
// concurrent use and fast. A durable event reaches a sink after it is journaled,
// so a sink can miss the last event before a crash; (RunID, Seq) is unique, and
// the journal is the source to reconcile from.
type EventSink func(Event)

type ctxKey int

const (
	keySinks ctxKey = iota
	keyParent
	keyJournal
	keyRedactors
	keySignals
	keyDrain
	keyNote
	keySource
)

// WithEvents returns a context whose runs report to sink, in addition to any
// sinks already attached. Sub-agents inherit the context and therefore the
// sinks: one sink sees the whole multi-agent tree.
func WithEvents(ctx context.Context, sink EventSink) context.Context {
	prev, _ := ctx.Value(keySinks).([]EventSink)
	next := append(append([]EventSink(nil), prev...), sink)
	return context.WithValue(ctx, keySinks, next)
}

// Emit sends ev to every sink on ctx. Multi-agent patterns call it for their
// ephemeral steps. No redactor runs here, so put step labels in Text and never
// content.
func Emit(ctx context.Context, ev Event) {
	sinks, _ := ctx.Value(keySinks).([]EventSink)
	if len(sinks) == 0 {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if p, ok := ctx.Value(keyParent).(parentInfo); ok && ev.RunID == "" {
		ev.ParentRun, ev.RootRun = p.runID, p.root
		ev.Depth = p.depth + 1
	}
	for _, s := range sinks {
		s(ev)
	}
}

// parentInfo is what a run leaves in the context of its tools so that a
// sub-agent started from a tool knows its parent, root and depth.
type parentInfo struct {
	runID string
	root  string
	depth int
	meta  map[string]string
}

// WithParent marks ctx as belonging to runID (a run, or a pattern acting as
// one): runs and events started with the returned context are its children, one
// level deeper.
func WithParent(ctx context.Context, runID string) context.Context {
	root := runID
	var meta map[string]string
	if p, ok := ctx.Value(keyParent).(parentInfo); ok {
		root, meta = p.root, p.meta
	}
	return context.WithValue(ctx, keyParent, parentInfo{runID: runID, root: root, depth: Depth(ctx), meta: meta})
}

// Depth returns the nesting depth of a run started with ctx: 0 at the top, 1
// inside a tool of a top-level run, and so on.
func Depth(ctx context.Context) int {
	if p, ok := ctx.Value(keyParent).(parentInfo); ok {
		return p.depth + 1
	}
	return 0
}

// ParentRun returns the ID of the run whose tool (or pattern step) ctx belongs
// to, or "".
func ParentRun(ctx context.Context) string {
	if p, ok := ctx.Value(keyParent).(parentInfo); ok {
		return p.runID
	}
	return ""
}

// Signals returns the signals delivered to the tool call ctx belongs to. A tool
// that parked uses them on re-invocation. multi.Subagent forwards them to its
// child's Input.
func Signals(ctx context.Context) []Signal {
	s, _ := ctx.Value(keySignals).([]Signal)
	return s
}

// WithDrain returns a context for graceful shutdown and the function that starts
// it. After drain is called, durable runs on the context finish their current
// step, record it and return ErrDrained, resumable by any process.
func WithDrain(ctx context.Context) (_ context.Context, drain func()) {
	ch := make(chan struct{})
	closed := false
	return context.WithValue(ctx, keyDrain, ch), func() {
		if !closed {
			closed = true
			close(ch)
		}
	}
}

func draining(ctx context.Context) bool {
	ch, _ := ctx.Value(keyDrain).(chan struct{})
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// IsCancel reports whether this event cancels its run. A cancel reaches every run
// below, which no other stop does, so a store that projects Lineage looks for this.
func (e Event) IsCancel() bool { return e.Kind == KindRunStopping && e.Stop == StopCancelled }

// redact rewrites every string an event carries, in place on deep copies.
// visit is the one path text takes to the journal, the history, the sinks and
// the hooks. TestRedactionCoversEveryString fails the build's tests if a new
// string field is added to an event payload and left out of this list.
func (e *Event) redact(visit func(*string)) {
	visit(&e.Text)
	e.Messages = redactMessages(e.Messages, visit)
	e.Pinned = redactMessages(e.Pinned, visit)
	if e.Meta != nil {
		meta := make(map[string]string, len(e.Meta))
		for k, v := range e.Meta {
			visit(&v)
			meta[k] = v
		}
		e.Meta = meta
	}
	if e.Pin != nil {
		p := *e.Pin
		visit(&p.Instructions)
		visit(&p.Shape)
		e.Pin = &p
	}
	if e.Edit != nil {
		ed := *e.Edit
		ed.With = redactMessages(ed.With, visit)
		e.Edit = &ed
	}
	if e.Reply != nil {
		r := *e.Reply
		visit(&r.Text)
		r.ToolCalls = redactCalls(r.ToolCalls, visit)
		r.Opaque = redactRaw(r.Opaque, visit)
		e.Reply = &r
	}
	if e.Failure != nil {
		f := *e.Failure
		visit(&f.Detail)
		e.Failure = &f
	}
	if e.Decision != nil {
		d := *e.Decision
		visit(&d.Reason)
		e.Decision = &d
	}
	if e.Handoff != nil {
		h := *e.Handoff
		visit(&h.Note)
		e.Handoff = &h
	}
	if e.Waits != nil {
		w := append([]Wait(nil), e.Waits...)
		for i := range w {
			visit(&w[i].Detail)
		}
		e.Waits = w
	}
	if e.Signal != nil {
		s := *e.Signal
		visit(&s.Value)
		e.Signal = &s
	}
	if e.Verdict != nil {
		v := *e.Verdict
		visit(&v.Feedback)
		visit(&v.Rewrite)
		e.Verdict = &v
	}
}

func redactMessages(ms []model.Message, visit func(*string)) []model.Message {
	if ms == nil {
		return nil
	}
	out := append([]model.Message(nil), ms...)
	for i := range out {
		visit(&out[i].Content)
		out[i].ToolCalls = redactCalls(out[i].ToolCalls, visit)
		out[i].Opaque = redactRaw(out[i].Opaque, visit)
	}
	return out
}

func redactCalls(cs []model.ToolCall, visit func(*string)) []model.ToolCall {
	if cs == nil {
		return nil
	}
	out := append([]model.ToolCall(nil), cs...)
	for i := range out {
		out[i].Arguments = redactRaw(out[i].Arguments, visit)
	}
	return out
}

func redactRaw(raw json.RawMessage, visit func(*string)) json.RawMessage {
	if raw == nil {
		return nil
	}
	s := string(raw)
	visit(&s)
	return json.RawMessage(s)
}
