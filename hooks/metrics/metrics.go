// Package metrics records aggregate run metrics: counts, token totals and
// latency distributions per model, tool and stop reason.
//
// The Registry is a small, dependency-free in-memory store with Snapshot for
// dashboards and tests. To export to Prometheus or OpenTelemetry, implement
// Recorder over your client and pass it to Sink instead.
//
// It is a sink, not a hook, like trace. A hook can repeat after a crash and would count
// twice, and it never sees a call that failed or was lost. A sink gets each durable
// event once. It can still miss the last event before a crash: the journal is the
// source to reconcile a total from.
//
// Metric names (labels in braces):
//
//	agent_runs_total{agent,stop}         counter
//	agent_run_duration_ms{agent}         histogram
//	agent_turns{agent}                   histogram
//	model_calls_total{model}             counter
//	model_errors_total{model,class}      counter
//	model_lost_total{agent}              counter
//	model_latency_ms{model}              histogram
//	model_tokens_total{model,kind}       counter (kind = input|output)
//	tool_calls_total{tool,outcome}       counter (outcome = ok|error|denied|lost|...)
//	tool_latency_ms{tool}                histogram
package metrics

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
)

// Recorder is the sink the hook writes to.
type Recorder interface {
	Add(name string, labels map[string]string, delta float64)
	Observe(name string, labels map[string]string, value float64)
}

// Registry is an in-memory Recorder. Safe for concurrent use.
type Registry struct {
	mu       sync.Mutex
	counters map[string]float64
	hists    map[string][]float64
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{counters: map[string]float64{}, hists: map[string][]float64{}}
}

// Key renders a metric name with sorted labels, e.g. `tool_calls_total{outcome=ok,tool=echo}`.
func Key(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + labels[k]
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

// Add implements Recorder.
func (r *Registry) Add(name string, labels map[string]string, delta float64) {
	r.mu.Lock()
	r.counters[Key(name, labels)] += delta
	r.mu.Unlock()
}

// Observe implements Recorder.
func (r *Registry) Observe(name string, labels map[string]string, v float64) {
	r.mu.Lock()
	k := Key(name, labels)
	r.hists[k] = append(r.hists[k], v)
	r.mu.Unlock()
}

// Summary describes one histogram.
type Summary struct {
	Count int     `json:"count"`
	Sum   float64 `json:"sum"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	Max   float64 `json:"max"`
}

// Snapshot is a point-in-time copy of all metrics.
type Snapshot struct {
	Counters   map[string]float64 `json:"counters"`
	Histograms map[string]Summary `json:"histograms"`
}

// Snapshot returns the current values.
func (r *Registry) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Snapshot{Counters: map[string]float64{}, Histograms: map[string]Summary{}}
	for k, v := range r.counters {
		s.Counters[k] = v
	}
	for k, vs := range r.hists {
		sorted := slices.Clone(vs)
		slices.Sort(sorted)
		sum := 0.0
		for _, v := range sorted {
			sum += v
		}
		s.Histograms[k] = Summary{Count: len(sorted), Sum: sum,
			P50: quantile(sorted, .5), P95: quantile(sorted, .95), Max: sorted[len(sorted)-1]}
	}
	return s
}

func quantile(sorted []float64, q float64) float64 {
	i := int(q * float64(len(sorted)-1))
	return sorted[i]
}

// Sink returns an event sink writing to r. Pass it to harness.WithEvents.
func Sink(r Recorder) harness.EventSink {
	tokens := func(name string, u model.Usage) {
		r.Add("model_tokens_total", map[string]string{"model": name, "kind": "input"}, float64(u.Input))
		r.Add("model_tokens_total", map[string]string{"model": name, "kind": "output"}, float64(u.Output))
	}
	return func(e harness.Event) {
		name := modelName(e.Model)
		switch e.Kind {
		case harness.KindModelDone:
			r.Add("model_calls_total", map[string]string{"model": name}, 1)
			r.Observe("model_latency_ms", map[string]string{"model": name}, ms(e.Duration))
			tokens(name, e.Reply.Usage)
		case harness.KindModelFailed:
			r.Add("model_calls_total", map[string]string{"model": name}, 1)
			r.Add("model_errors_total", map[string]string{"model": name, "class": string(e.Failure.Class)}, 1)
			r.Observe("model_latency_ms", map[string]string{"model": name}, ms(e.Duration))
			tokens(name, e.Failure.Usage)
		case harness.KindModelLost:
			r.Add("model_lost_total", map[string]string{"agent": e.Agent}, 1)
		case harness.KindToolGated:
			if e.Decision != nil && e.Decision.Action == harness.ActionDeny {
				r.Add("tool_calls_total", map[string]string{"tool": e.Tool, "outcome": "denied"}, 1)
			}
		case harness.KindToolDone:
			r.Add("tool_calls_total", map[string]string{"tool": e.Tool, "outcome": e.Outcome}, 1)
			r.Observe("tool_latency_ms", map[string]string{"tool": e.Tool}, ms(e.Duration))
		case harness.KindToolLost:
			r.Add("tool_calls_total", map[string]string{"tool": e.Tool, "outcome": "lost"}, 1)
		case harness.KindRunEnded:
			agent := map[string]string{"agent": e.Agent}
			r.Add("agent_runs_total", map[string]string{"agent": e.Agent, "stop": e.Outcome}, 1)
			r.Observe("agent_run_duration_ms", agent, ms(e.Duration))
			r.Observe("agent_turns", agent, float64(e.Turn))
		}
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func modelName(m string) string {
	if m == "" {
		return "default"
	}
	return m
}

// String renders a snapshot as sorted lines, handy in examples.
func (s Snapshot) String() string {
	var lines []string
	for k, v := range s.Counters {
		lines = append(lines, fmt.Sprintf("%s %g", k, v))
	}
	for k, h := range s.Histograms {
		lines = append(lines, fmt.Sprintf("%s count=%d", k, h.Count))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
