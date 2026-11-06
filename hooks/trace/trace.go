// Package trace turns a run's events into OpenTelemetry-shaped spans, without
// depending on the OpenTelemetry SDK.
//
// Spans use the OpenTelemetry GenAI semantic-convention names
// (gen_ai.operation.name, gen_ai.request.model, gen_ai.usage.input_tokens,
// gen_ai.tool.name, ...), carry trace and span IDs in the W3C format, and are
// handed to an Exporter. An adapter to the real OTel SDK is a dozen lines:
// implement Exporter and replay each Span onto an OTel tracer.
//
// Tracing is a sink, not a hook. Attach it once on the context, and every run in the
// tree reports to it. Span IDs are hashes of the run and step, so a span's identity
// survives a resume in another process, and the tree is whole across a crash.
package trace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
)

// SpanData is what an Exporter receives once a span has ended.
type SpanData struct {
	TraceID  string         `json:"trace_id"`
	SpanID   string         `json:"span_id"`
	ParentID string         `json:"parent_id,omitempty"`
	Name     string         `json:"name"`
	Start    time.Time      `json:"start"`
	End      time.Time      `json:"end,omitzero"`
	Attrs    map[string]any `json:"attrs,omitempty"`
	Status   string         `json:"status"` // "ok" or "error"
	Error    string         `json:"error,omitempty"`
}

// Exporter receives finished spans. Must be safe for concurrent use.
type Exporter interface {
	Export(SpanData)
}

// ExporterFunc adapts a function to Exporter.
type ExporterFunc func(SpanData)

// Export calls f.
func (f ExporterFunc) Export(s SpanData) { f(s) }

func id(n int, parts ...any) string {
	h := sha256.Sum256(fmt.Append(nil, parts...))
	return hex.EncodeToString(h[:n])
}

func runSpan(run string) string {
	if run == "" {
		return ""
	}
	return id(8, "run", run)
}

// folder holds what turning events into spans needs to remember between them:
// when each run and wait began, and the calls of the latest model reply per run.
// Sink and FromEvents both run on it, so the two cannot drift.
type folder struct {
	content Content
	starts  map[string]time.Time
	opened  map[string]time.Time
	calls   map[string][]model.ToolCall
}

func newFolder(opts []Option) *folder {
	var c config
	for _, o := range opts {
		o(&c)
	}
	return &folder{content: c.content, starts: map[string]time.Time{}, opened: map[string]time.Time{}, calls: map[string][]model.ToolCall{}}
}

// Content says whether span attributes carry message and argument text. The
// default, ContentNone, exports structure, timing, tokens and outcomes only: what
// leaves the process through an exporter is never conversation content unless a
// consumer opts in. ContentFull adds the model's reply text, tool arguments, tool
// results, the value a wait was answered with and a failed run's error text.
type Content int

// The two settings of Content.
const (
	ContentNone Content = iota
	ContentFull
)

type config struct{ content Content }

// Option configures Sink, SinkWith and FromEvents.
type Option func(*config)

// WithContent sets whether spans carry content. The default is ContentNone.
func WithContent(c Content) Option { return func(o *config) { o.content = c } }

// span folds one event in and returns the span it ends, if any.
func (f *folder) span(e harness.Event) (SpanData, bool) {
	if e.Seq == 0 {
		return SpanData{}, false
	}
	for _, w := range e.Waits {
		if _, ok := f.opened[w.ID]; !ok {
			f.opened[w.ID] = e.Time
		}
	}
	root := e.RootRun
	if root == "" {
		root = e.RunID
	}
	s := SpanData{TraceID: id(16, "trace", root), ParentID: runSpan(e.RunID), Start: e.Time.Add(-e.Duration), End: e.Time, Status: "ok"}
	switch e.Kind {
	case harness.KindRunStarted, harness.KindRunClaimed:
		if _, ok := f.starts[e.RunID]; !ok {
			f.starts[e.RunID] = e.Time
		}
		return SpanData{}, false
	case harness.KindModelDone, harness.KindModelFailed:
		s.SpanID, s.Name = id(8, "model", e.RunID, e.Turn, e.Attempt), "chat"
		s.Attrs = map[string]any{"gen_ai.operation.name": "chat", "gen_ai.request.model": e.Model, "harness.turn": e.Turn}
		// FirstToken: set gen_ai.server.time_to_first_token here once Event carries it.
		if e.Reply != nil {
			s.Attrs["gen_ai.usage.input_tokens"] = e.Reply.Usage.Input
			s.Attrs["gen_ai.usage.output_tokens"] = e.Reply.Usage.Output
			s.Attrs["gen_ai.response.finish_reasons"] = []string{string(e.Reply.Finish)}
		}
		if e.Failure != nil {
			s.Status, s.Error = "error", string(e.Failure.Class)
		}
		if e.Kind == harness.KindModelDone && e.Reply != nil {
			f.calls[e.RunID] = e.Reply.ToolCalls
			if f.content == ContentFull {
				s.Attrs["gen_ai.output.text"] = e.Reply.Text
			}
		}
	case harness.KindToolDone:
		s.SpanID, s.Name = id(8, "tool", e.RunID, e.Turn, e.Index), "execute_tool "+e.Tool
		s.Attrs = map[string]any{"gen_ai.operation.name": "execute_tool", "gen_ai.tool.name": e.Tool, "harness.tool.outcome": e.Outcome}
		if e.Outcome == "error" {
			s.Status = "error"
		}
		if f.content == ContentFull {
			s.Attrs["gen_ai.tool.call.result"] = e.Text
			if calls := f.calls[e.RunID]; e.Index >= 0 && e.Index < len(calls) {
				s.Attrs["gen_ai.tool.call.arguments"] = string(calls[e.Index].Arguments)
			}
		}
	case harness.KindWaitResolved:
		if e.Signal == nil {
			return SpanData{}, false
		}
		s.SpanID, s.Name = id(8, "wait", e.RunID, e.Signal.Wait), "wait "+e.Signal.Kind
		s.Start = e.Time
		if at, ok := f.opened[e.Signal.Wait]; ok {
			s.Start = at
			delete(f.opened, e.Signal.Wait)
		}
		s.Attrs = map[string]any{"harness.wait.kind": e.Signal.Kind, "harness.wait.expired": e.Signal.Expired}
		if f.content == ContentFull {
			s.Attrs["harness.wait.value"] = e.Signal.Value
		}
	case harness.KindRunEnded:
		if at, ok := f.starts[e.RunID]; ok {
			s.Start = at
			delete(f.starts, e.RunID)
		}
		s.SpanID, s.ParentID, s.Name = runSpan(e.RunID), runSpan(e.ParentRun), "invoke_agent "+e.Agent
		s.Attrs = map[string]any{"gen_ai.operation.name": "invoke_agent", "gen_ai.agent.name": e.Agent,
			"harness.run_id": e.RunID, "harness.depth": e.Depth, "harness.stop": e.Outcome}
		if e.To == harness.StateFailed {
			s.Status, s.Error = "error", e.Outcome
			if f.content == ContentFull {
				s.Error = e.Text
			}
		}
	default:
		return SpanData{}, false
	}
	return s, true
}

// FromEvents builds the spans of a run, or of a tree of runs, from their recorded
// events: the same ids and attributes the live Sink produces, so a page, an export
// job or an eval reads a finished run the way an exporter saw it live. It is pure:
// no clock, no I/O. Pass the events of every run in the tree in the order a sink
// would have seen them to get the whole tree. A run span starts at the run's first
// event in events, where the live sink starts it at the process that began it.
// Tool spans carry their outcome. A wait becomes a span of its own, from the event
// that opened it to the one that resolved it.
func FromEvents(events []harness.Event, opts ...Option) []SpanData {
	f := newFolder(opts)
	var out []SpanData
	for _, e := range events {
		if s, ok := f.span(e); ok {
			out = append(out, s)
		}
	}
	return out
}

// Sink returns an event sink that exports a span per run, model attempt, tool call
// and wait. Pass it to harness.WithEvents. A run span's start is known only to the
// process that saw it start, so after a resume it starts at the resuming worker.
func Sink(exporters ...Exporter) harness.EventSink { return SinkWith(nil, exporters...) }

// SinkWith is Sink with options. Spans carry no message, argument or result text
// unless opts include WithContent(ContentFull).
func SinkWith(opts []Option, exporters ...Exporter) harness.EventSink {
	var (
		mu sync.Mutex
		f  = newFolder(opts)
	)
	return func(e harness.Event) {
		mu.Lock()
		s, ok := f.span(e)
		mu.Unlock()
		if !ok {
			return
		}
		for _, x := range exporters {
			x.Export(s)
		}
	}
}

// Memory is an Exporter that keeps spans in memory. Safe for concurrent use.
type Memory struct {
	mu    sync.Mutex
	spans []SpanData
}

// Export implements Exporter.
func (m *Memory) Export(s SpanData) {
	m.mu.Lock()
	m.spans = append(m.spans, s)
	m.mu.Unlock()
}

// Spans returns the exported spans in end order.
func (m *Memory) Spans() []SpanData {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]SpanData(nil), m.spans...)
}

// JSONL returns an Exporter that writes one JSON object per span to w.
func JSONL(w io.Writer) Exporter {
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	return ExporterFunc(func(s SpanData) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(s)
	})
}
