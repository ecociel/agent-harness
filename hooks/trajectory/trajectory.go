// Package trajectory records complete runs — messages, tool outcomes, state path and
// usage — for evals, replay and debugging. Most agent failures are context failures,
// and a trajectory shows what the model saw at every turn.
//
// A trajectory is a pure view of a run's events. From builds one from any Run, so a run
// can be read back from a journal long after it ended, across any number of crashes.
// Recorder collects them as runs end. AfterRun is at least once, so Write can repeat a
// run after a crash; key on RunID.
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
	Outcome  string          `json:"outcome"`
	Denied   bool            `json:"denied,omitempty"`
	Duration time.Duration   `json:"duration_ns"`
}

// Trajectory is one recorded run.
type Trajectory struct {
	RunID     string             `json:"run_id"`
	ParentRun string             `json:"parent_run,omitempty"`
	Agent     string             `json:"agent"`
	Input     []model.Message    `json:"input"`
	Path      []harness.State    `json:"path"`
	Steps     []Step             `json:"steps"`
	Messages  []model.Message    `json:"messages"`
	Output    string             `json:"output"`
	Stop      harness.StopReason `json:"stop"`
	Detail    string             `json:"detail,omitempty"`
	Usage     model.Usage        `json:"usage"`
	Turns     int                `json:"turns"`
	Claims    int                `json:"claims"`
}

// From builds the trajectory of r. Its Events must be set, which they are on a Run
// from Fold, and on what a hook is given.
func From(r harness.Run) Trajectory {
	t := Trajectory{RunID: r.ID, ParentRun: r.ParentRun, Agent: r.Agent, Path: r.Path, Messages: r.Messages,
		Output: r.Output, Stop: r.Stop, Detail: r.Detail, Usage: r.Usage, Turns: r.Turn, Claims: r.Claims}
	var calls []model.ToolCall
	args := func(i int) json.RawMessage {
		if i < len(calls) {
			return calls[i].Arguments
		}
		return nil
	}
	for _, e := range r.Events {
		switch e.Kind {
		case harness.KindRunStarted:
			t.Input = e.Messages
		case harness.KindModelDone:
			calls = e.Reply.ToolCalls
		case harness.KindToolGated:
			if e.Decision != nil && e.Decision.Action == harness.ActionDeny {
				t.Steps = append(t.Steps, Step{Turn: e.Turn, Tool: e.Tool, Args: args(e.Index), Result: e.Text, Outcome: "denied", Denied: true})
			}
		case harness.KindToolDone:
			t.Steps = append(t.Steps, Step{Turn: e.Turn, Tool: e.Tool, Args: args(e.Index), Result: e.Text, Outcome: e.Outcome, Duration: e.Duration})
		case harness.KindToolLost:
			t.Steps = append(t.Steps, Step{Turn: e.Turn, Tool: e.Tool, Args: args(e.Index), Outcome: "lost"})
		}
	}
	return t
}

// Recorder is the hook. Safe for concurrent use.
type Recorder struct {
	Write io.Writer // optional JSONL sink for finished trajectories

	mu   sync.Mutex
	done []Trajectory
}

// New returns an empty recorder.
func New() *Recorder { return &Recorder{} }

// Name implements harness.Hook.
func (rec *Recorder) Name() string { return "trajectory" }

// AfterRun implements harness.AfterRunHook.
func (rec *Recorder) AfterRun(_ context.Context, r harness.Run) {
	t := From(r)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.done = append(rec.done, t)
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

// Get returns the latest finished trajectory for runID.
func (rec *Recorder) Get(runID string) (Trajectory, bool) {
	all := rec.All()
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].RunID == runID {
			return all[i], true
		}
	}
	return Trajectory{}, false
}
