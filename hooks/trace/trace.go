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

// Sink returns an event sink that exports a span per run, model attempt and tool
// call. Pass it to harness.WithEvents. A run span's start is known only to the
// process that saw it start, so after a resume it starts at the resuming worker.
func Sink(exporters ...Exporter) harness.EventSink {
	var (
		mu     sync.Mutex
		starts = map[string]time.Time{}
	)
	return func(e harness.Event) {
		if e.Seq == 0 {
			return
		}
		root := e.RootRun
		if root == "" {
			root = e.RunID
		}
		s := SpanData{TraceID: id(16, "trace", root), ParentID: runSpan(e.RunID), Start: e.Time.Add(-e.Duration), End: e.Time, Status: "ok"}
		switch e.Kind {
		case harness.KindRunStarted, harness.KindRunClaimed:
			mu.Lock()
			if _, ok := starts[e.RunID]; !ok {
				starts[e.RunID] = e.Time
			}
			mu.Unlock()
			return
		case harness.KindModelDone, harness.KindModelFailed:
			s.SpanID, s.Name = id(8, "model", e.RunID, e.Turn, e.Attempt), "chat"
			s.Attrs = map[string]any{"gen_ai.operation.name": "chat", "harness.turn": e.Turn}
			if e.Reply != nil {
				s.Attrs["gen_ai.usage.input_tokens"] = e.Reply.Usage.Input
				s.Attrs["gen_ai.usage.output_tokens"] = e.Reply.Usage.Output
				s.Attrs["gen_ai.response.finish_reasons"] = []string{string(e.Reply.Finish)}
			}
			if e.Failure != nil {
				s.Status, s.Error = "error", string(e.Failure.Class)
			}
		case harness.KindToolDone:
			s.SpanID, s.Name = id(8, "tool", e.RunID, e.Turn, e.Index), "execute_tool "+e.Tool
			s.Attrs = map[string]any{"gen_ai.operation.name": "execute_tool", "gen_ai.tool.name": e.Tool}
			if e.Outcome == "error" {
				s.Status = "error"
			}
		case harness.KindRunEnded:
			mu.Lock()
			if at, ok := starts[e.RunID]; ok {
				s.Start = at
				delete(starts, e.RunID)
			}
			mu.Unlock()
			s.SpanID, s.ParentID, s.Name = runSpan(e.RunID), runSpan(e.ParentRun), "invoke_agent "+e.Agent
			s.Attrs = map[string]any{"gen_ai.operation.name": "invoke_agent", "gen_ai.agent.name": e.Agent,
				"harness.run_id": e.RunID, "harness.depth": e.Depth, "harness.stop": e.Outcome}
			if e.To == harness.StateFailed {
				s.Status, s.Error = "error", e.Text
			}
		default:
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
