package multi

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/tool"
)

// Subagent turns a Runner into a delegation tool, the orchestrator-worker
// pattern: a lead agent decides at runtime what to delegate.
//
//	lead ──tool call──▶ subagent (fresh context) ──result──▶ lead
//
// As in Cursor's subagents, the sub-agent starts with a clean context: it
// sees only the task text the lead writes, so the lead must put everything
// needed into it, and the sub-agent's exploration never fills the lead's
// window. Because delegation is an ordinary tool, every policy hook on the
// lead (guard, approval, budget) applies to it too. Depth is capped so
// delegation cannot recurse forever.
type Subagent struct {
	Name        string
	Description string
	Agent       harness.Runner
	// MaxDepth is the deepest nesting at which this tool may still run
	// (default 3).
	MaxDepth int
	// Risk is the tool risk seen by policy hooks (default RiskRead).
	Risk tool.Risk
}

var taskSchema = json.RawMessage(`{"type":"object","properties":{"task":{"type":"string",` +
	`"description":"Complete, self-contained instructions; the sub-agent sees nothing else."}},"required":["task"]}`)

func (sa Subagent) maxDepth() int {
	if sa.MaxDepth <= 0 {
		return 3
	}
	return sa.MaxDepth
}

// Tool returns the foreground delegation tool: it blocks until the
// sub-agent finishes and returns its answer.
func (sa Subagent) Tool() tool.Tool {
	spec := tool.Spec{Name: sa.Name, Description: sa.Description, Parameters: taskSchema, Risk: sa.Risk}
	return tool.Func(spec, func(ctx context.Context, raw json.RawMessage) (string, error) {
		task, err := parseTask(raw)
		if err != nil {
			return "", err
		}
		return sa.delegate(ctx, task)
	})
}

func (sa Subagent) delegate(ctx context.Context, task string) (string, error) {
	if d := harness.Depth(ctx); d > sa.maxDepth() {
		return "", fmt.Errorf("delegation depth %d exceeds limit %d", d, sa.maxDepth())
	}
	s := open(ctx, "subagent", sa.Name)
	s.emit("delegate", task, nil)
	res, err := s.run(sa.Agent, harness.Prompt(task))
	if err != nil {
		_, _ = s.fail(harness.Prompt(task), err)
		return "", err
	}
	_, _ = s.done(harness.Prompt(task), res.Output, res.Stop, res.Detail, nil)
	if !res.OK() {
		return fmt.Sprintf("sub-agent %s stopped (%s): %s\npartial output: %s", sa.Name, res.Stop, res.Detail, res.Output), nil
	}
	return res.Output, nil
}

func parseTask(raw json.RawMessage) (string, error) {
	var a struct {
		Task string `json:"task"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Task == "" {
		return "", fmt.Errorf("want {\"task\": \"…\"}, got %s", raw)
	}
	return a.Task, nil
}

// WithSubagents returns a copy of lead whose tools also include one
// delegation tool per sub-agent.
func WithSubagents(lead *harness.Agent, subs ...Subagent) (*harness.Agent, error) {
	extra := make([]tool.Tool, len(subs))
	for i, s := range subs {
		extra[i] = s.Tool()
	}
	reg, err := lead.Tools.With(extra...)
	if err != nil {
		return nil, err
	}
	cp := *lead
	cp.Tools = reg
	return &cp, nil
}

// Background runs sub-agents asynchronously (Cursor's background
// subagents): start_<name> returns a job id immediately, and collect returns
// the job's status or result. The lead can start several jobs in one turn and
// keep working while they run. Safe for concurrent use.
type Background struct {
	subs []Subagent

	mu   sync.Mutex
	jobs map[string]*job
	n    int
}

type job struct {
	done   chan struct{}
	output string
	err    error
}

// NewBackground returns a background pool over subs.
func NewBackground(subs ...Subagent) *Background {
	return &Background{subs: subs, jobs: map[string]*job{}}
}

// Tools returns start_<name> for each sub-agent plus collect.
func (b *Background) Tools() []tool.Tool {
	var out []tool.Tool
	for _, sa := range b.subs {
		spec := tool.Spec{Name: "start_" + sa.Name, Description: "Start in the background: " + sa.Description,
			Parameters: taskSchema, Risk: sa.Risk}
		out = append(out, tool.Func(spec, func(ctx context.Context, raw json.RawMessage) (string, error) {
			task, err := parseTask(raw)
			if err != nil {
				return "", err
			}
			return b.start(ctx, sa, task), nil
		}))
	}
	collect := tool.Spec{Name: "collect", Description: "Get the result of a background job. Set wait=true to block until it finishes.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"wait":{"type":"boolean"}},"required":["id"]}`)}
	out = append(out, tool.Func(collect, func(ctx context.Context, raw json.RawMessage) (string, error) {
		var a struct {
			ID   string `json:"id"`
			Wait bool   `json:"wait"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", err
		}
		return b.collect(ctx, a.ID, a.Wait)
	}))
	return out
}

func (b *Background) start(ctx context.Context, sa Subagent, task string) string {
	b.mu.Lock()
	b.n++
	id := fmt.Sprintf("job-%d", b.n)
	j := &job{done: make(chan struct{})}
	b.jobs[id] = j
	b.mu.Unlock()
	// The job outlives the tool call (and the turn) that started it, but
	// keeps the context's values: event sinks, tracer, depth and parent.
	ctx = context.WithoutCancel(ctx)
	go func() {
		defer close(j.done)
		j.output, j.err = sa.delegate(ctx, task)
	}()
	return fmt.Sprintf(`{"job":%q,"status":"running"}`, id)
}

func (b *Background) collect(ctx context.Context, id string, wait bool) (string, error) {
	b.mu.Lock()
	j, ok := b.jobs[id]
	b.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("no job %q", id)
	}
	if wait {
		select {
		case <-j.done:
		case <-ctx.Done():
			return "", context.Cause(ctx)
		}
	}
	select {
	case <-j.done:
		if j.err != nil {
			return "", j.err
		}
		return j.output, nil
	default:
		return fmt.Sprintf(`{"job":%q,"status":"running"}`, id), nil
	}
}

// Wait blocks until every started job has finished (for tests and clean
// shutdown).
func (b *Background) Wait() {
	b.mu.Lock()
	jobs := make([]*job, 0, len(b.jobs))
	for _, j := range b.jobs {
		jobs = append(jobs, j)
	}
	b.mu.Unlock()
	for _, j := range jobs {
		<-j.done
	}
}
