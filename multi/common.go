package multi

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/trace"
	"github.com/ecociel/agent-harness/model"
)

var patternSeq atomic.Int64

// scope is one execution of a pattern: its id, its context for children and
// its event/trace plumbing.
type scope struct {
	id    string
	name  string
	kind  string
	depth int
	ctx   context.Context // children run with this context
	span  *trace.Span
	start time.Time

	mu         sync.Mutex // children may finish concurrently
	usage      model.Usage
	turns      int
	modelCalls int
	toolCalls  int
}

// add accumulates a child's consumption.
func (s *scope) add(res *harness.Result) {
	if res == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage = s.usage.Add(res.Usage)
	s.turns += res.Turns
	s.modelCalls += res.ModelCalls
	s.toolCalls += res.ToolCalls
}

func open(ctx context.Context, kind, name string) *scope {
	if name == "" {
		name = kind
	}
	s := &scope{id: fmt.Sprintf("%s-%d", name, patternSeq.Add(1)), name: name, kind: kind, depth: harness.Depth(ctx), start: time.Now()}
	ctx, s.span = trace.Start(ctx, "pattern "+kind, map[string]any{"harness.pattern": kind, "harness.pattern.name": name})
	s.ctx = harness.WithParent(ctx, s.id)
	harness.Emit(ctx, harness.Event{Kind: harness.KindPattern, RunID: s.id, ParentRun: harness.ParentRun(ctx),
		Agent: name, Depth: s.depth, Hook: kind, Outcome: "start"})
	return s
}

// emit reports one step of the pattern.
func (s *scope) emit(step, text string, data map[string]any) {
	harness.Emit(s.ctx, harness.Event{Kind: harness.KindPattern, RunID: s.id, Agent: s.name,
		Depth: s.depth, Hook: s.kind, Outcome: step, Text: text, Data: data})
	s.span.AddEvent(step, map[string]any{"text": text})
}

// run runs a child and accumulates its usage.
func (s *scope) run(r harness.Runner, in harness.Input) (*harness.Result, error) {
	res, err := r.Run(s.ctx, in)
	s.add(res)
	return res, err
}

// done builds the pattern's Result and closes the span.
func (s *scope) done(in harness.Input, output string, stop harness.StopReason, detail string, err error) (*harness.Result, error) {
	s.span.SetAttr("harness.stop", string(stop))
	s.span.Finish(err)
	s.mu.Lock()
	usage, turns, mc, tc := s.usage, s.turns, s.modelCalls, s.toolCalls
	s.mu.Unlock()
	s.emit("end", detail, map[string]any{"stop": stop, "usage": usage})
	state := harness.StateDone
	switch {
	case err != nil:
		state = harness.StateFailed
	case stop != harness.StopCompleted:
		state = harness.StateStopped
	}
	return &harness.Result{
		RunID: s.id, Agent: s.name, Output: output, Stop: stop, Detail: detail, State: state,
		Usage: usage, Turns: turns, ModelCalls: mc, ToolCalls: tc, Duration: time.Since(s.start),
		Messages: append(append([]model.Message(nil), in.Messages...), model.Assistant(output)),
	}, err
}

func (s *scope) ok(in harness.Input, output string) (*harness.Result, error) {
	return s.done(in, output, harness.StopCompleted, "", nil)
}

func (s *scope) fail(in harness.Input, err error) (*harness.Result, error) {
	return s.done(in, "", harness.StopFailed, err.Error(), err)
}

// stopped reports a child that did not complete.
func (s *scope) stopped(in harness.Input, who string, res *harness.Result) (*harness.Result, error) {
	return s.done(in, res.Output, res.Stop, fmt.Sprintf("%s stopped: %s %s", who, res.Stop, res.Detail), nil)
}

var winnerRE = regexp.MustCompile(`(?i)winner\s*[:=]\s*#?(\d+)`)

// parseWinner reads "WINNER: n" (1-based) from a judge's answer. It returns
// a 0-based index and false if no valid choice was found.
func parseWinner(text string, n int) (int, bool) {
	m := winnerRE.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	i, err := strconv.Atoi(m[1])
	if err != nil || i < 1 || i > n {
		return 0, false
	}
	return i - 1, true
}

func numbered(title string, outputs []string) string {
	var b strings.Builder
	for i, o := range outputs {
		fmt.Fprintf(&b, "### %s %d\n%s\n\n", title, i+1, strings.TrimSpace(o))
	}
	return b.String()
}
