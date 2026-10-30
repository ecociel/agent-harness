package multi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/tool"
)

const bodyTool = "pattern"

// ScriptedModel is the Pin.Model of a pattern's lead. Its model events are not paid
// calls, so a consumer projecting spend from the journal skips runs pinned to it.
const ScriptedModel = "multi.scripted"

// pattern is what every multi-agent pattern has in common: it is a run. A lead agent
// whose model is a script calls one tool, the pattern's body, and answers with what
// the body returned. That gives a pattern everything a run has with no code of its
// own: its input survives in the journal, its waits are recorded and validated, a
// finished pattern answers from its journal, and a sweep finds it.
//
// The body keeps no state. It is run again after a crash or a signal, and it walks
// its children, whose IDs come from their position: finished ones return from their
// journals and the rest carry on.
type pattern struct {
	kind, name string
	journal    harness.Journal
	hooks      []harness.Hook
	parts      []string // the children, in order: a resume must find them unchanged
}

// run advances the pattern. Its parts are the tool's description, so they are pinned
// with the run, and a resume that finds them changed fails instead of giving a
// child's journal to a different agent.
func (p pattern) run(ctx context.Context, in harness.Input, body func(*scope) (string, error)) (*harness.Run, error) {
	if p.name == "" {
		p.name = p.kind
	}
	spec := tool.Spec{Name: bodyTool, Effect: tool.EffectIdempotent, Description: p.kind + ": " + strings.Join(p.parts, ", ")}
	lead := &harness.Agent{Name: p.name, Model: ScriptedModel, Journal: p.journal, Hooks: p.hooks, Provider: script{},
		Tools: tool.NewRegistry(tool.Func(spec, func(ctx context.Context, args json.RawMessage) (string, error) {
			var msgs []model.Message
			if err := json.Unmarshal(args, &msgs); err != nil {
				return "", err
			}
			s := &scope{id: harness.ParentRun(ctx), name: p.name, kind: p.kind, ctx: ctx,
				in: harness.Input{Messages: msgs}, signals: harness.Signals(ctx)}
			return body(s)
		}))}
	res, err := lead.Run(ctx, in)
	if res == nil {
		return nil, err
	}
	return settled(res), err
}

// script is the model of a pattern's lead: call the body with the input, then answer
// with its output. The input travels as the call's arguments, which is how a resume
// that carries no Messages gets it back.
type script struct{}

func (script) Complete(_ context.Context, req model.Request) iter.Seq2[model.Event, error] {
	last := req.Messages[len(req.Messages)-1]
	if last.Role != model.RoleTool {
		args, err := json.Marshal(req.Messages)
		if err != nil {
			return model.Fail(err)
		}
		return model.Stream(model.Response{ToolCalls: []model.ToolCall{{ID: bodyTool, Name: bodyTool, Arguments: args}}})
	}
	t, ok := parseTally(last.Content)
	if !ok {
		return model.Fail(errors.New(last.Content)) // the call was denied: its parts changed
	}
	return model.Stream(model.Response{Text: t.Output})
}

// tally is the body's report, recorded as its tool result. A pattern's lead pays
// nothing, so what a caller wants to know is the sum over its children.
type tally struct {
	Output     string             `json:"output"`
	Stop       harness.StopReason `json:"stop"`
	Usage      model.Usage        `json:"usage"`
	Turns      int                `json:"turns"`
	ModelCalls int                `json:"model_calls"`
	LostCalls  int                `json:"lost_calls"`
	ToolCalls  int                `json:"tool_calls"`
}

func parseTally(s string) (tally, bool) {
	var t tally
	return t, json.Unmarshal([]byte(s), &t) == nil && t.Stop != ""
}

// settled turns the lead's run into the pattern's: the children's totals and a
// transcript of input and answer, without the lead's scripted turns.
func settled(res *harness.Run) *harness.Run {
	out := *res
	for i := len(res.Events) - 1; i >= 0; i-- {
		e := res.Events[i]
		if t, ok := parseTally(e.Text); ok && e.Kind == harness.KindToolDone {
			out.Usage, out.Turn, out.ModelCalls, out.LostCalls, out.ToolCalls = t.Usage, t.Turns, t.ModelCalls, t.LostCalls, t.ToolCalls
			out.Output = t.Output
			out.Messages = append(slices.Clone(res.Events[0].Messages), model.Assistant(t.Output))
			break
		}
	}
	return &out
}

// scope is one execution of a pattern's body.
type scope struct {
	id, name, kind string
	ctx            context.Context // children run with this context
	in             harness.Input   // the pattern's input, from the journal
	signals        []harness.Signal

	mu sync.Mutex // children may finish concurrently
	t  tally
}

// fork names a child by its position in the pattern. Goroutines take their ID from
// the loop that starts them, so an ID never depends on which one ran first.
func (s *scope) fork(part string) string { return s.id + "/" + part }

// emit reports one step of the pattern. No redactor runs here, so text is a label.
func (s *scope) emit(step, text string) {
	harness.Emit(s.ctx, harness.Event{Kind: harness.KindPattern, Agent: s.name, Hook: s.kind, Outcome: step, Text: text})
}

// advance brings a child as far as it can go, then offers it the signals if it is
// parked. Asking first is safe: a parked or finished child writes nothing. A child
// refuses a signal that is not its own and writes nothing, so this is how a signal
// finds its wait at any depth of nesting.
//
// A nil Run means the worker stopped, not that the child failed, so the error goes
// up as a stop and never reads as a failed step.
func (s *scope) advance(r harness.Runner, in harness.Input) (*harness.Run, error) {
	res, err := r.Run(s.ctx, in)
	if res != nil && res.State == harness.StateSuspended && len(s.signals) > 0 {
		res, err = deliver(s.ctx, r, harness.Input{RunID: in.RunID, Signals: s.signals})
	}
	if res == nil {
		return nil, stopped(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.t.Usage = s.t.Usage.Add(res.Usage)
	s.t.Turns += res.Turn
	s.t.ModelCalls += res.ModelCalls
	s.t.LostCalls += res.LostCalls
	s.t.ToolCalls += res.ToolCalls
	return res, err
}

// deliver hands signals to a child. A child refuses a whole delivery if one signal is
// not for it, or if its clock passed a wait's expiry after its parent took the answer.
// So the signals go again one at a time, which saves the good ones, and a last call
// with none lets the child record any expiry. A refused signal writes nothing.
func deliver(ctx context.Context, r harness.Runner, in harness.Input) (*harness.Run, error) {
	res, err := r.Run(ctx, in)
	if !errors.Is(err, harness.ErrStaleSignal) {
		return res, err
	}
	signals := in.Signals
	in.Signals = nil
	for _, sig := range signals {
		one := in
		one.Signals = []harness.Signal{sig}
		if res, err = r.Run(ctx, one); err != nil && !errors.Is(err, harness.ErrStaleSignal) {
			return res, err
		}
	}
	return r.Run(ctx, in)
}

func stopped(err error) error {
	if errors.Is(err, harness.ErrFenced) || errors.Is(err, harness.ErrDrained) {
		return err
	}
	return fmt.Errorf("%w: %w", harness.ErrDrained, err)
}

// finish ends the body. A pattern that did not complete stops its own run with the
// same reason, and the tally still goes on record.
func (s *scope) finish(output string, stop harness.StopReason, detail string) (string, error) {
	s.t.Output, s.t.Stop = output, stop
	if stop == harness.StopCompleted {
		return s.report(), nil
	}
	return s.report(), harness.Stop(stop, detail)
}

func (s *scope) ok(output string) (string, error) {
	return s.finish(output, harness.StopCompleted, "")
}

func (s *scope) fail(err error) (string, error) {
	return s.finish("", harness.StopFailed, err.Error())
}

// stoppedChild reports a child that did not complete.
func (s *scope) stoppedChild(who string, res *harness.Run) (string, error) {
	return s.finish(res.Output, res.Stop, fmt.Sprintf("%s stopped: %s %s", who, res.Stop, res.Detail))
}

// park suspends the pattern on its children's waits.
func (s *scope) park(waits ...harness.Wait) (string, error) {
	s.t.Stop = harness.StopSuspended
	return s.report(), harness.Suspend(waits...)
}

func (s *scope) report() string {
	b, _ := json.Marshal(s.t)
	return string(b)
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

func names(rs []harness.Runner) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = harness.NameOf(r)
	}
	return out
}
