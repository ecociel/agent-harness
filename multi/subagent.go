package multi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
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
//
// The child's run ID is the lead's run ID, "/" and the call's idempotency key, so a
// repeated call reaches the same child, a finished child answers from its journal,
// and a store that routes runs by a prefix of the ID keeps the child with its lead.
// That is why the tool is EffectIdempotent whatever the child does: the child's own
// tools carry their own classes. A child's waits surface at the root, and a signal
// sent there comes back down through this tool.
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
	spec := tool.Spec{Name: sa.Name, Description: sa.Description, Parameters: taskSchema, Risk: sa.Risk,
		Effect: tool.EffectIdempotent}
	return childTool{Tool: tool.Func(spec, func(ctx context.Context, raw json.RawMessage) (string, error) {
		task, err := parseTask(raw)
		if err != nil {
			return "", err
		}
		return sa.delegate(ctx, task)
	}), child: sa.Agent}
}

// childTool is a tool that starts the run childID names.
type childTool struct {
	tool.Tool
	child  harness.Runner
	prefix string
}

// CancelChild implements harness.ChildTool.
func (t childTool) CancelChild(ctx context.Context, reason string) error {
	return cancelChild(ctx, t.child, childID(ctx, t.prefix), reason)
}

// childID names the run a tool call starts: the calling run's ID, "/", prefix and the
// call's key. It derives from the call, so a retry or a resume reaches the same child.
func childID(ctx context.Context, prefix string) string {
	id := prefix + tool.IdempotencyKey(ctx)
	if parent := harness.ParentRun(ctx); parent != "" {
		return parent + "/" + id
	}
	return id
}

func (sa Subagent) delegate(ctx context.Context, task string) (string, error) {
	if d := harness.Depth(ctx); d > sa.maxDepth() {
		return "", fmt.Errorf("delegation depth %d exceeds limit %d", d, sa.maxDepth())
	}
	in := harness.Prompt(task)
	in.RunID, in.Signals = childID(ctx, ""), harness.Signals(ctx)
	harness.Emit(ctx, harness.Event{Kind: harness.KindPattern, Agent: sa.Name, Hook: "subagent", Outcome: "delegate"})
	res, err := deliver(ctx, sa.Agent, in)
	return sa.result(res, err)
}

// result maps a child's run onto what its tool call returns. A child that stopped or
// parked stops or parks the call, so the lead's model never hears of either.
func (sa Subagent) result(res *harness.Run, err error) (string, error) {
	switch {
	case res == nil:
		return "", stopped(err)
	case res.State == harness.StateSuspended:
		return "", harness.Suspend(res.Waits...)
	case !res.OK():
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
//
// A job's id is its child's run ID, so a job survives this value and this process.
// collect on an id it does not know attaches to the child's journal. start returns
// only once the child's input is durable, so a crash cannot leave the lead holding
// the id of a job that never existed.
type Background struct {
	subs []Subagent

	mu   sync.Mutex
	jobs map[string]*job
}

type job struct {
	done chan struct{}
	res  *harness.Run
	err  error
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
			Parameters: taskSchema, Risk: sa.Risk, Effect: tool.EffectIdempotent}
		out = append(out, childTool{Tool: tool.Func(spec, func(ctx context.Context, raw json.RawMessage) (string, error) {
			task, err := parseTask(raw)
			if err != nil {
				return "", err
			}
			return b.start(ctx, sa, task), nil
		}), child: sa.Agent, prefix: jobPrefix(sa)})
	}
	collect := tool.Spec{Name: "collect", Description: "Get the result of a background job. Set wait=true to block until it finishes.",
		Effect:     tool.EffectRead,
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

// launch runs a job, or finds the one already running.
func (b *Background) launch(ctx context.Context, sa Subagent, in harness.Input) *job {
	b.mu.Lock()
	defer b.mu.Unlock()
	if j, ok := b.jobs[in.RunID]; ok {
		return j
	}
	j := &job{done: make(chan struct{})}
	b.jobs[in.RunID] = j
	// The job outlives the tool call (and the turn) that started it, but
	// keeps the context's values: event sinks, journal, depth and parent.
	ctx = context.WithoutCancel(ctx)
	go func() {
		defer close(j.done)
		j.res, j.err = deliver(ctx, sa.Agent, in)
	}()
	return j
}

func (b *Background) start(ctx context.Context, sa Subagent, task string) string {
	in := harness.Prompt(task)
	in.RunID = childID(ctx, jobPrefix(sa))
	durable := make(chan struct{})
	var once sync.Once
	ctx = harness.WithEvents(ctx, func(e harness.Event) {
		if e.RunID == in.RunID && e.Seq > 0 {
			once.Do(func() { close(durable) })
		}
	})
	j := b.launch(ctx, sa, in)
	select {
	case <-durable:
	case <-j.done:
	}
	return fmt.Sprintf(`{"job":%q,"status":"running"}`, in.RunID)
}

func jobPrefix(sa Subagent) string { return sa.Name + "/" }

// jobName reads the sub-agent's name from a job id, the segment before the key.
func jobName(id string) string {
	i := strings.LastIndex(id, "/")
	if i < 0 {
		return ""
	}
	return id[strings.LastIndex(id[:i], "/")+1 : i]
}

func (b *Background) collect(ctx context.Context, id string, wait bool) (string, error) {
	i := slices.IndexFunc(b.subs, func(sa Subagent) bool { return jobName(id) == sa.Name })
	if i < 0 {
		return "", fmt.Errorf("no job %q", id)
	}
	sa := b.subs[i]
	j := b.launch(ctx, sa, harness.Input{RunID: id, Signals: harness.Signals(ctx)})
	if wait {
		select {
		case <-j.done:
		case <-ctx.Done():
			return "", context.Cause(ctx)
		}
	}
	select {
	case <-j.done:
	default:
		return fmt.Sprintf(`{"job":%q,"status":"running"}`, id), nil
	}
	b.mu.Lock()
	delete(b.jobs, id) // the journal holds the outcome, and the next collect asks it
	b.mu.Unlock()
	if errors.Is(j.err, harness.ErrNoSuchRun) {
		return "", fmt.Errorf("no job %q", id) // the id came from the model
	}
	return sa.result(j.res, j.err)
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
