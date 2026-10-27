// Package trace records OpenTelemetry-shaped spans for runs, model calls and
// tool calls, without depending on the OpenTelemetry SDK.
//
// Spans use the OpenTelemetry GenAI semantic-convention names
// (gen_ai.operation.name, gen_ai.request.model, gen_ai.usage.input_tokens,
// gen_ai.tool.name, ...), carry trace and span IDs in the W3C format, and are
// handed to an Exporter when they end. An adapter to the real OTel SDK is a
// dozen lines: implement Exporter and replay each Span onto an OTel tracer.
//
// The tree mirrors execution: a run span ("invoke_agent <name>") parents its
// model spans ("chat <model>") and tool spans ("execute_tool <tool>"). The
// tool span is put into the tool's context, so a sub-agent started from a
// delegation tool parents its own run span under that tool span: the trace
// stays one tree across agents. State transitions are recorded as span
// events on the run span.
//
// Multi-agent patterns open their own spans with Start, which is a no-op when
// no Tracer is in the context.
package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
)

// SpanData is the exported, immutable form of a span: what an Exporter
// receives once the span has ended.
type SpanData struct {
	TraceID  string         `json:"trace_id"`
	SpanID   string         `json:"span_id"`
	ParentID string         `json:"parent_id,omitempty"`
	Name     string         `json:"name"`
	Start    time.Time      `json:"start"`
	End      time.Time      `json:"end,omitzero"`
	Attrs    map[string]any `json:"attrs,omitempty"`
	Events   []SpanEvent    `json:"events,omitempty"`
	Status   string         `json:"status"` // "ok" or "error"
	Error    string         `json:"error,omitempty"`
}

// Span is a live span. Use its methods while it is open; Finish exports a
// SpanData snapshot. A nil *Span is valid and does nothing.
type Span struct {
	SpanData

	tracer *Tracer
	mu     sync.Mutex
	ended  bool
}

// SpanEvent is a timestamped annotation on a span.
type SpanEvent struct {
	Name  string         `json:"name"`
	Time  time.Time      `json:"time"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// SetAttr sets one attribute. Nil-safe.
func (s *Span) SetAttr(k string, v any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.Attrs == nil {
		s.Attrs = map[string]any{}
	}
	s.Attrs[k] = v
	s.mu.Unlock()
}

// AddEvent appends a span event. Nil-safe.
func (s *Span) AddEvent(name string, attrs map[string]any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.Events = append(s.Events, SpanEvent{Name: name, Time: time.Now(), Attrs: attrs})
	s.mu.Unlock()
}

// Finish ends the span (once) and exports it. Nil-safe.
func (s *Span) Finish(err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.End = time.Now()
	s.Status = "ok"
	if err != nil {
		s.Status, s.Error = "error", err.Error()
	}
	snap := s.copyLocked()
	s.mu.Unlock()
	s.tracer.export(snap)
}

func (s *Span) copyLocked() SpanData {
	attrs := make(map[string]any, len(s.Attrs))
	for k, v := range s.Attrs {
		attrs[k] = v
	}
	return SpanData{TraceID: s.TraceID, SpanID: s.SpanID, ParentID: s.ParentID, Name: s.Name,
		Start: s.Start, End: s.End, Attrs: attrs, Events: append([]SpanEvent(nil), s.Events...),
		Status: s.Status, Error: s.Error}
}

// Exporter receives finished spans. Must be safe for concurrent use.
type Exporter interface {
	Export(SpanData)
}

// ExporterFunc adapts a function to Exporter.
type ExporterFunc func(SpanData)

// Export calls f.
func (f ExporterFunc) Export(s SpanData) { f(s) }

// Tracer creates spans and fans finished ones out to exporters.
type Tracer struct {
	exporters []Exporter
}

// NewTracer returns a tracer exporting to exps.
func NewTracer(exps ...Exporter) *Tracer { return &Tracer{exporters: exps} }

func (t *Tracer) export(s SpanData) {
	if t == nil {
		return
	}
	for _, e := range t.exporters {
		e.Export(s)
	}
}

type ctxKey int

const (
	keyTracer ctxKey = iota
	keySpan
)

// WithTracer attaches t to ctx. Runs, patterns and the Hook use it.
func WithTracer(ctx context.Context, t *Tracer) context.Context {
	return context.WithValue(ctx, keyTracer, t)
}

// TracerFrom returns the tracer on ctx, or nil.
func TracerFrom(ctx context.Context) *Tracer {
	t, _ := ctx.Value(keyTracer).(*Tracer)
	return t
}

// SpanFrom returns the current span on ctx, or nil.
func SpanFrom(ctx context.Context) *Span {
	s, _ := ctx.Value(keySpan).(*Span)
	return s
}

// Start opens a child of the current span (or a new trace) using the tracer
// on ctx. Without a tracer it returns ctx and a nil span, whose methods are
// no-ops, so callers never need to check.
func Start(ctx context.Context, name string, attrs map[string]any) (context.Context, *Span) {
	return TracerFrom(ctx).start(ctx, name, attrs)
}

func (t *Tracer) start(ctx context.Context, name string, attrs map[string]any) (context.Context, *Span) {
	if t == nil {
		return ctx, nil
	}
	s := &Span{SpanData: SpanData{Name: name, Start: time.Now(), Attrs: map[string]any{}, SpanID: randHex(8)}, tracer: t}
	for k, v := range attrs {
		s.Attrs[k] = v
	}
	if p := SpanFrom(ctx); p != nil {
		s.TraceID, s.ParentID = p.TraceID, p.SpanID
	} else {
		s.TraceID = randHex(16)
	}
	return context.WithValue(ctx, keySpan, s), s
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Hook traces runs it is attached to. If Tracer is nil, the tracer on the
// run's context is used; with neither, the hook does nothing.
type Hook struct {
	Tracer *Tracer
}

// New returns a hook using t.
func New(t *Tracer) *Hook { return &Hook{Tracer: t} }

// Name implements harness.Hook.
func (h *Hook) Name() string { return "trace" }

func (h *Hook) tracer(ctx context.Context) *Tracer {
	if h.Tracer != nil {
		return h.Tracer
	}
	return TracerFrom(ctx)
}

const runSpanKey = "trace.span"

func runSpan(r *harness.Run) *Span {
	s, _ := r.Value(runSpanKey).(*Span)
	return s
}

// BeforeRun opens the run span under whatever span is current on ctx.
func (h *Hook) BeforeRun(ctx context.Context, r *harness.Run) error {
	_, s := h.tracer(ctx).start(ctx, "invoke_agent "+r.Agent.Name, map[string]any{
		"gen_ai.operation.name": "invoke_agent",
		"gen_ai.agent.name":     r.Agent.Name,
		"harness.run_id":        r.ID,
		"harness.depth":         r.Depth,
	})
	r.Set(runSpanKey, s)
	return nil
}

// OnTransition records each state change as a span event.
func (h *Hook) OnTransition(_ context.Context, r *harness.Run, from, to harness.State) {
	runSpan(r).AddEvent("state "+string(to), map[string]any{"from": string(from), "turn": r.Turn})
}

// WrapModel opens a "chat <model>" span around the provider call.
func (h *Hook) WrapModel(r *harness.Run, next harness.ModelCall) harness.ModelCall {
	return func(ctx context.Context, req model.Request) (model.Response, error) {
		ctx = withSpan(ctx, runSpan(r))
		ctx, s := h.tracer(ctx).start(ctx, "chat "+req.Model, map[string]any{
			"gen_ai.operation.name": "chat",
			"gen_ai.request.model":  req.Model,
			"harness.turn":          r.Turn,
		})
		resp, err := next(ctx, req)
		s.SetAttr("gen_ai.usage.input_tokens", resp.Usage.Input)
		s.SetAttr("gen_ai.usage.output_tokens", resp.Usage.Output)
		s.SetAttr("gen_ai.response.finish_reasons", []string{string(resp.Finish)})
		s.Finish(err)
		return resp, err
	}
}

// WrapTool opens an "execute_tool <name>" span and puts it into the tool's
// context so sub-agents nest under it.
func (h *Hook) WrapTool(r *harness.Run, next harness.ToolCallFunc) harness.ToolCallFunc {
	return func(ctx context.Context, call model.ToolCall) (string, error) {
		ctx = withSpan(ctx, runSpan(r))
		ctx, s := h.tracer(ctx).start(ctx, "execute_tool "+call.Name, map[string]any{
			"gen_ai.operation.name": "execute_tool",
			"gen_ai.tool.name":      call.Name,
			"gen_ai.tool.call.id":   call.ID,
		})
		out, err := next(ctx, call)
		s.SetAttr("harness.result_bytes", len(out))
		s.Finish(err)
		return out, err
	}
}

// AfterRun closes the run span.
func (h *Hook) AfterRun(_ context.Context, r *harness.Run, res *harness.Result) {
	s := runSpan(r)
	s.SetAttr("harness.stop", string(res.Stop))
	s.SetAttr("harness.turns", res.Turns)
	s.SetAttr("gen_ai.usage.input_tokens", res.Usage.Input)
	s.SetAttr("gen_ai.usage.output_tokens", res.Usage.Output)
	var err error
	if res.State == harness.StateFailed {
		err = errString(res.Detail)
	}
	s.Finish(err)
}

func withSpan(ctx context.Context, s *Span) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, keySpan, s)
}

type errString string

func (e errString) Error() string { return string(e) }

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
