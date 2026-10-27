package harness

import (
	"context"
	"sync/atomic"
	"time"
)

// EventKind classifies an Event.
type EventKind string

// Event kinds.
const (
	// KindRunStart and KindRunEnd bracket one Agent.Run.
	KindRunStart EventKind = "run.start"
	KindRunEnd   EventKind = "run.end"
	// KindTransition is one state-machine edge (From → To).
	KindTransition EventKind = "transition"
	// KindHook is one hook invocation at Point, with Outcome and Duration.
	KindHook EventKind = "hook"
	// KindDelta is one streamed text chunk (Text).
	KindDelta EventKind = "delta"
	// KindModel is one completed model call (Data: usage, finish, tool calls).
	KindModel EventKind = "model"
	// KindTool is one completed tool call (Hook holds the tool name).
	KindTool EventKind = "tool"
	// KindPattern is emitted by multi-agent patterns for their own steps
	// (fan-out, judge verdicts, handoffs, board claims).
	KindPattern EventKind = "pattern"
)

// Event is the unit of observability. Every state change and every hook
// invocation produces one, so a sink sees the complete execution of a run,
// including nested sub-agents (which carry Depth > 0 and ParentRun).
type Event struct {
	Seq       int64          `json:"seq"`
	Time      time.Time      `json:"time"`
	Kind      EventKind      `json:"kind"`
	RunID     string         `json:"run_id,omitempty"`
	ParentRun string         `json:"parent_run,omitempty"`
	Agent     string         `json:"agent,omitempty"`
	Depth     int            `json:"depth"`
	Turn      int            `json:"turn,omitempty"`
	From      State          `json:"from,omitempty"`
	To        State          `json:"to,omitempty"`
	Hook      string         `json:"hook,omitempty"`
	Point     Point          `json:"point,omitempty"`
	Outcome   string         `json:"outcome,omitempty"`
	Duration  time.Duration  `json:"duration_ns,omitempty"`
	Text      string         `json:"text,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// EventSink receives events. It is called synchronously from the run (and,
// for parallel patterns, from several goroutines), so it must be safe for
// concurrent use and fast; buffer if you need to do I/O.
type EventSink func(Event)

type ctxKey int

const (
	keySinks ctxKey = iota
	keyParent
)

var eventSeq atomic.Int64

// WithEvents returns a context whose runs report to sink, in addition to any
// sinks already attached. Sub-agents inherit the context and therefore the
// sinks: one sink sees the whole multi-agent tree.
func WithEvents(ctx context.Context, sink EventSink) context.Context {
	prev, _ := ctx.Value(keySinks).([]EventSink)
	next := append(append([]EventSink(nil), prev...), sink)
	return context.WithValue(ctx, keySinks, next)
}

// Emit sends ev to every sink on ctx. It fills Seq and Time. Patterns and
// custom hooks may call it to add their own events.
func Emit(ctx context.Context, ev Event) {
	sinks, _ := ctx.Value(keySinks).([]EventSink)
	if len(sinks) == 0 {
		return
	}
	ev.Seq = eventSeq.Add(1)
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if p, ok := ctx.Value(keyParent).(parentInfo); ok && ev.RunID == "" {
		ev.ParentRun = p.runID
		ev.Depth = p.depth + 1
	}
	for _, s := range sinks {
		s(ev)
	}
}

// parentInfo is what a run leaves in the context of its tools so that a
// sub-agent started from a tool knows its parent and depth.
type parentInfo struct {
	runID string
	depth int
}

// WithParent marks ctx as belonging to runID (a run, or a pattern acting as
// one): runs and events started with the returned context are its children,
// one level deeper. Multi-agent patterns use it so their sub-agents nest
// under them in the event stream.
func WithParent(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, keyParent, parentInfo{runID: runID, depth: Depth(ctx)})
}

// Depth returns the nesting depth of a run started with ctx: 0 at the top,
// 1 inside a tool of a top-level run, and so on. Delegation tools use it to
// cap recursion.
func Depth(ctx context.Context) int {
	if p, ok := ctx.Value(keyParent).(parentInfo); ok {
		return p.depth + 1
	}
	return 0
}

// ParentRun returns the ID of the run whose tool (or pattern step) ctx
// belongs to, or "".
func ParentRun(ctx context.Context) string {
	if p, ok := ctx.Value(keyParent).(parentInfo); ok {
		return p.runID
	}
	return ""
}
