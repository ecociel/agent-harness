// Package trajectory records complete runs — messages, tool outcomes, state
// path and usage — for evals, replay and debugging. The talk's point that
// most agent failures are context failures is why this exists: a trajectory
// shows exactly what the model saw at every turn.
package trajectory

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
)

// Step is one tool outcome inside a trajectory.
type Step struct {
	Turn     int             `json:"turn"`
	Tool     string          `json:"tool"`
	Args     json.RawMessage `json:"args"`
	Result   string          `json:"result"`
	Denied   bool            `json:"denied,omitempty"`
	Error    string          `json:"error,omitempty"`
	Duration time.Duration   `json:"duration_ns"`
}

// Trajectory is one recorded run.
type Trajectory struct {
	RunID     string          `json:"run_id"`
	ParentRun string          `json:"parent_run,omitempty"`
	Agent     string          `json:"agent"`
	Input     []model.Message `json:"input"`
	Steps     []Step          `json:"steps"`
	Result    *harness.Result `json:"result"`
}

// Recorder is the hook. Safe for concurrent use.
type Recorder struct {
	mu    sync.Mutex
	open  map[string]*Trajectory
	done  []Trajectory
	Write io.Writer // optional JSONL sink for finished trajectories
}

// New returns an empty recorder.
func New() *Recorder { return &Recorder{open: map[string]*Trajectory{}} }

// Name implements harness.Hook.
func (rec *Recorder) Name() string { return "trajectory" }

// BeforeRun opens a trajectory.
func (rec *Recorder) BeforeRun(_ context.Context, r *harness.Run) error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.open == nil {
		rec.open = map[string]*Trajectory{}
	}
	rec.open[r.ID] = &Trajectory{RunID: r.ID, ParentRun: r.ParentRun, Agent: r.Agent.Name,
		Input: append([]model.Message(nil), r.Messages...)}
	return nil
}

// AfterTool appends a step.
func (rec *Recorder) AfterTool(_ context.Context, r *harness.Run, out harness.ToolOutcome) error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if t := rec.open[r.ID]; t != nil {
		s := Step{Turn: r.Turn, Tool: out.Call.Name, Args: out.Call.Arguments, Result: out.Content,
			Denied: out.Denied, Duration: out.Duration}
		if out.Err != nil {
			s.Error = out.Err.Error()
		}
		t.Steps = append(t.Steps, s)
	}
	return nil
}

// AfterRun closes the trajectory.
func (rec *Recorder) AfterRun(_ context.Context, r *harness.Run, res *harness.Result) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	t := rec.open[r.ID]
	if t == nil {
		return
	}
	delete(rec.open, r.ID)
	t.Result = res
	rec.done = append(rec.done, *t)
	if rec.Write != nil {
		_ = json.NewEncoder(rec.Write).Encode(t)
	}
}

// All returns finished trajectories in completion order.
func (rec *Recorder) All() []Trajectory {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]Trajectory(nil), rec.done...)
}

// Get returns the finished trajectory for runID.
func (rec *Recorder) Get(runID string) (Trajectory, bool) {
	for _, t := range rec.All() {
		if t.RunID == runID {
			return t, true
		}
	}
	return Trajectory{}, false
}
