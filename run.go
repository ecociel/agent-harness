package harness

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/tool"
)

// Defaults for zero-valued Agent limits.
const (
	DefaultMaxTurns   = 8
	DefaultMaxRepairs = 2
)

// ErrIllegalTransition is returned (wrapped) if the machine is asked to take
// an edge that is not in the transition table. It indicates a harness bug.
var ErrIllegalTransition = errors.New("harness: illegal transition")

// Run executes one run of the agent: the outer loop around as many inner
// model/tool turns as needed.
//
// A run that ends on purpose (guardrail, budget, max turns, handoff,
// cancellation) returns its Result and a nil error; inspect Result.Stop. Only
// StateFailed (provider or hook errors) returns a non-nil error, together with
// the partial Result.
func (a *Agent) Run(ctx context.Context, in Input) (*Result, error) {
	if a.Provider == nil {
		return nil, errors.New("harness: agent has no Provider")
	}
	m := &machine{
		a:     a,
		hooks: indexHooks(a.Hooks),
		run: &Run{
			ID:        newRunID(a.Name),
			ParentRun: ParentRun(ctx),
			Agent:     a,
			Depth:     Depth(ctx),
			State:     StateStart,
			Messages:  append([]model.Message(nil), in.Messages...),
			Started:   time.Now(),
		},
		path: []State{StateStart},
	}
	return m.execute(ctx)
}

// machine drives one Run through the states. Each state method does its
// work and returns the next state; execute validates the edge, emits the
// transition and calls the next method. Keeping "what happens in a state" and
// "which edges exist" separate is what makes the loop readable.
type machine struct {
	a     *Agent
	hooks hookSet
	run   *Run
	path  []State
	req   model.Request // built in Compose, sent in Model

	stop    StopReason
	detail  string
	err     error
	output  string
	handoff *tool.Handoff
	// hookText is a default Event.Text for the hook invocation in progress
	// (the tool name and reason of a decision, a verdict's feedback).
	hookText string
}

func (m *machine) execute(ctx context.Context) (*Result, error) {
	r := m.run
	m.emit(ctx, Event{Kind: KindRunStart, Text: model.LastUser(r.Messages)})
	state := StateStart
	for !state.Terminal() {
		var next State
		if err := context.Cause(ctx); err != nil {
			next = m.stopWith(StopCancelled, err.Error())
		} else {
			next = m.step(ctx, state)
		}
		if !CanTransition(state, next) {
			m.err = fmt.Errorf("%w %s → %s", ErrIllegalTransition, state, next)
			next = StateFailed
		}
		m.transition(ctx, state, next)
		state = next
	}
	res := m.result()
	for _, h := range m.hooks.afterRun {
		_ = m.invoke(ctx, h, PointAfterRun, func() (string, error) {
			h.AfterRun(ctx, r, res)
			return "ok", nil
		})
	}
	m.emit(ctx, Event{Kind: KindRunEnd, Outcome: string(res.Stop), Text: res.Output,
		Duration: res.Duration, Data: map[string]any{
			"state": res.State, "usage": res.Usage, "turns": res.Turns, "detail": res.Detail,
		}})
	if res.State == StateFailed {
		return res, m.err
	}
	return res, nil
}

func (m *machine) step(ctx context.Context, s State) State {
	switch s {
	case StateStart:
		return StatePrepare
	case StatePrepare:
		return m.prepare(ctx)
	case StateCompose:
		return m.compose(ctx)
	case StateModel:
		return m.callModel(ctx)
	case StateRoute:
		if len(m.run.Last.ToolCalls) > 0 {
			return StateTools
		}
		return StateVerify
	case StateTools:
		return m.tools(ctx)
	case StateObserve:
		return m.observe(ctx)
	case StateVerify:
		return m.verify(ctx)
	}
	m.err = fmt.Errorf("harness: no step for state %q", s)
	return StateFailed
}

// prepare: BeforeRun hooks.
func (m *machine) prepare(ctx context.Context) State {
	for _, h := range m.hooks.beforeRun {
		err := m.invoke(ctx, h, PointBeforeRun, func() (string, error) {
			return "ok", h.BeforeRun(ctx, m.run)
		})
		if err != nil {
			return m.fromErr(err)
		}
	}
	return StateCompose
}

// compose: enforce MaxTurns, build the request and run BeforeModel hooks.
// The finished request is handed to the Model state through m.req.
func (m *machine) compose(ctx context.Context) State {
	r := m.run
	maxTurns := m.a.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	if r.Turn >= maxTurns {
		return m.stopWith(StopMaxTurns, fmt.Sprintf("reached %d turns", maxTurns))
	}
	r.Turn++
	req := m.request()
	for _, h := range m.hooks.beforeModel {
		err := m.invoke(ctx, h, PointBeforeModel, func() (string, error) {
			return "ok", h.BeforeModel(ctx, r, &req)
		})
		if err != nil {
			return m.fromErr(err)
		}
	}
	m.req = req
	return StateModel
}

func (m *machine) request() model.Request {
	r := m.run
	msgs := make([]model.Message, 0, len(r.Messages)+1)
	if m.a.Instructions != "" {
		msgs = append(msgs, model.System(m.a.Instructions))
	}
	msgs = append(msgs, r.Messages...)
	var specs []model.ToolSpec
	for _, s := range m.a.Tools.Specs() {
		specs = append(specs, s.ModelSpec())
	}
	return model.Request{Model: m.a.Model, Messages: msgs, Tools: specs,
		MaxTokens: m.a.MaxTokens, Temperature: m.a.Temperature}
}

// callModel: the WrapModel chain around the provider stream, OnDelta for
// every streamed event, then AfterModel hooks.
func (m *machine) callModel(ctx context.Context) State {
	r := m.run
	req := m.req
	base := func(ctx context.Context, req model.Request) (model.Response, error) {
		seq := m.a.Provider.Complete(ctx, req)
		return model.Collect(seq, func(ev model.Event) {
			if d, ok := ev.(model.TextDelta); ok {
				m.emit(ctx, Event{Kind: KindDelta, Text: d.Text})
			}
			for _, h := range m.hooks.onDelta {
				h.OnDelta(ctx, r, ev)
			}
		})
	}
	call := ModelCall(base)
	for i := len(m.hooks.wrapModel) - 1; i >= 0; i-- {
		call = m.instrumentModel(m.hooks.wrapModel[i], call)
	}
	started := time.Now()
	resp, err := call(ctx, req)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return m.stopWith(StopCancelled, cause.Error())
		}
		return m.fromErr(fmt.Errorf("model call: %w", err))
	}
	for _, h := range m.hooks.onDelta {
		m.emit(ctx, Event{Kind: KindHook, Hook: h.Name(), Point: PointOnDelta, Outcome: "streamed", Text: m.run.takeNote(h.Name())})
	}
	for _, h := range m.hooks.afterModel {
		err := m.invoke(ctx, h, PointAfterModel, func() (string, error) {
			return "ok", h.AfterModel(ctx, r, &resp)
		})
		if err != nil {
			r.Usage = r.Usage.Add(resp.Usage)
			r.ModelCalls++
			return m.fromErr(err)
		}
	}
	r.Usage = r.Usage.Add(resp.Usage)
	r.ModelCalls++
	r.Last = resp
	r.Messages = append(r.Messages, resp.Message())
	m.emit(ctx, Event{Kind: KindModel, Duration: time.Since(started), Text: resp.Text,
		Data: map[string]any{"usage": resp.Usage, "finish": resp.Finish,
			"tool_calls": len(resp.ToolCalls), "model": req.Model, "total_usage": r.Usage}})
	return StateRoute
}

func (m *machine) instrumentModel(h ModelMiddleware, next ModelCall) ModelCall {
	wrapped := h.WrapModel(m.run, next)
	return func(ctx context.Context, req model.Request) (model.Response, error) {
		start := time.Now()
		resp, err := wrapped(ctx, req)
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		m.emit(ctx, Event{Kind: KindHook, Hook: h.Name(), Point: PointWrapModel,
			Outcome: outcome, Duration: time.Since(start), Text: m.run.takeNote(h.Name())})
		return resp, err
	}
}

// tools: run each requested call through BeforeTool → WrapTool → AfterTool.
func (m *machine) tools(ctx context.Context) State {
	r := m.run
	toolCtx := context.WithValue(ctx, keyParent, parentInfo{runID: r.ID, depth: r.Depth})
	for _, call := range r.Last.ToolCalls {
		out, next, done := m.oneTool(toolCtx, call)
		r.Messages = append(r.Messages, model.ToolResult(call.ID, call.Name, out.Content))
		r.ToolCalls++
		for _, h := range m.hooks.afterTool {
			err := m.invoke(ctx, h, PointAfterTool, func() (string, error) {
				return "ok", h.AfterTool(ctx, r, out)
			})
			if err != nil && !done {
				next, done = m.fromErr(err), true
			}
		}
		data := map[string]any{"args": string(call.Arguments), "denied": out.Denied, "result": clip(out.Content, 400)}
		outcome := "ok"
		switch {
		case out.Denied:
			outcome = "denied"
		case out.Err != nil:
			outcome = "error"
		}
		m.emit(ctx, Event{Kind: KindTool, Hook: call.Name, Outcome: outcome, Duration: out.Duration, Text: out.Reason, Data: data})
		if done {
			return next
		}
	}
	return StateObserve
}

// oneTool returns the outcome, and (next, true) if the run must leave the
// Tools state immediately (halt or handoff).
func (m *machine) oneTool(ctx context.Context, call model.ToolCall) (ToolOutcome, State, bool) {
	r := m.run
	out := ToolOutcome{Call: call}
	for _, h := range m.hooks.beforeTool {
		var d Decision
		_ = m.invoke(ctx, h, PointBeforeTool, func() (string, error) {
			d = h.BeforeTool(ctx, r, call)
			m.hookText = call.Name
			if d.Reason != "" {
				m.hookText += ": " + d.Reason
			}
			return d.outcome(), nil
		})
		switch d.Action {
		case ActionDeny:
			out.Denied, out.Reason = true, d.Reason
			out.Content = fmt.Sprintf(`{"denied":true,"tool":%q,"reason":%q}`, call.Name, d.Reason)
			return out, "", false
		case ActionHalt:
			out.Denied, out.Reason = true, d.Reason
			out.Content = fmt.Sprintf(`{"denied":true,"tool":%q,"reason":%q}`, call.Name, d.Reason)
			reason := d.Stop
			if reason == "" {
				reason = StopGuardrail
			}
			return out, m.stopWith(reason, d.Reason), true
		}
	}
	exec := ToolCallFunc(func(ctx context.Context, c model.ToolCall) (string, error) {
		return m.a.Tools.Call(ctx, c.Name, c.Arguments)
	})
	for i := len(m.hooks.wrapTool) - 1; i >= 0; i-- {
		exec = m.instrumentTool(m.hooks.wrapTool[i], exec)
	}
	start := time.Now()
	content, err := exec(ctx, call)
	out.Duration = time.Since(start)
	if h, ok := tool.AsHandoff(err); ok {
		m.handoff = h
		out.Content = fmt.Sprintf(`{"handoff":%q}`, h.Target)
		return out, m.stopWith(StopHandoff, "to "+h.Target), true
	}
	if err != nil {
		out.Err = err
		out.Content = "error: " + err.Error()
		return out, "", false
	}
	out.Content = content
	return out, "", false
}

func (m *machine) instrumentTool(h ToolMiddleware, next ToolCallFunc) ToolCallFunc {
	wrapped := h.WrapTool(m.run, next)
	return func(ctx context.Context, call model.ToolCall) (string, error) {
		start := time.Now()
		out, err := wrapped(ctx, call)
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		text := call.Name
		if n := m.run.takeNote(h.Name()); n != "" {
			text = call.Name + ": " + n
		}
		m.emit(ctx, Event{Kind: KindHook, Hook: h.Name(), Point: PointWrapTool,
			Outcome: outcome, Duration: time.Since(start), Text: text})
		return out, err
	}
}

// observe: AfterTurn hooks at the turn boundary.
func (m *machine) observe(ctx context.Context) State {
	for _, h := range m.hooks.afterTurn {
		err := m.invoke(ctx, h, PointAfterTurn, func() (string, error) {
			return "ok", h.AfterTurn(ctx, m.run)
		})
		if err != nil {
			return m.fromErr(err)
		}
	}
	return StateCompose
}

// verify: CheckOutput hooks on the candidate answer.
func (m *machine) verify(ctx context.Context) State {
	r := m.run
	output := r.Last.Text
	for _, h := range m.hooks.output {
		var v Verdict
		_ = m.invoke(ctx, h, PointCheckOutput, func() (string, error) {
			v = h.CheckOutput(ctx, r, output)
			m.hookText = v.Feedback
			return v.outcome(), nil
		})
		switch v.Action {
		case VerdictBlock:
			return m.stopWith(StopGuardrail, v.Feedback)
		case VerdictRevise:
			maxRepairs := m.a.MaxRepairs
			if maxRepairs <= 0 {
				maxRepairs = DefaultMaxRepairs
			}
			if r.Repairs >= maxRepairs {
				m.output = output
				return m.stopWith(StopMaxRepairs, v.Feedback)
			}
			r.Repairs++
			r.Messages = append(r.Messages, model.User("Your previous answer was rejected by a check: "+
				v.Feedback+"\nPlease revise your answer."))
			return StateCompose
		}
		if v.Rewrite != "" {
			output = v.Rewrite
			if n := len(r.Messages); n > 0 && r.Messages[n-1].Role == model.RoleAssistant {
				r.Messages[n-1].Content = output
			}
		}
	}
	m.output = output
	m.stop = StopCompleted
	return StateDone
}

// invoke runs one hook body and emits a KindHook event for it.
func (m *machine) invoke(ctx context.Context, h Hook, p Point, fn func() (string, error)) error {
	start := time.Now()
	outcome, err := fn()
	ev := Event{Kind: KindHook, Hook: h.Name(), Point: p, Outcome: outcome, Duration: time.Since(start),
		Text: m.run.takeNote(h.Name())}
	if ev.Text == "" {
		ev.Text = m.hookText
	}
	m.hookText = ""
	if err != nil {
		ev.Outcome = "error"
		if s, ok := AsStop(err); ok {
			ev.Outcome = "stop"
			ev.Text = string(s.Reason) + ": " + s.Detail
		} else {
			ev.Text = err.Error()
		}
	}
	m.emit(ctx, ev)
	return err
}

func (m *machine) fromErr(err error) State {
	if s, ok := AsStop(err); ok {
		return m.stopWith(s.Reason, s.Detail)
	}
	m.err = err
	m.stop, m.detail = StopFailed, err.Error()
	return StateFailed
}

func (m *machine) stopWith(reason StopReason, detail string) State {
	m.stop, m.detail = reason, detail
	return StateStopped
}

func (m *machine) transition(ctx context.Context, from, to State) {
	r := m.run
	r.State = to
	m.path = append(m.path, to)
	m.emit(ctx, Event{Kind: KindTransition, From: from, To: to,
		Data: map[string]any{"loop": to.Loop()}})
	for _, h := range m.hooks.transition {
		start := time.Now()
		h.OnTransition(ctx, r, from, to)
		m.emit(ctx, Event{Kind: KindHook, Hook: h.Name(), Point: PointOnTransition, Outcome: "ok",
			Duration: time.Since(start), Text: string(from) + "→" + string(to)})
	}
}

func (m *machine) emit(ctx context.Context, ev Event) {
	r := m.run
	ev.RunID, ev.ParentRun, ev.Agent, ev.Depth, ev.Turn = r.ID, r.ParentRun, m.a.Name, r.Depth, r.Turn
	Emit(ctx, ev)
}

func (m *machine) result() *Result {
	r := m.run
	return &Result{
		RunID:      r.ID,
		Agent:      m.a.Name,
		Output:     m.output,
		Messages:   append([]model.Message(nil), r.Messages...),
		State:      r.State,
		Stop:       m.stop,
		Detail:     m.detail,
		Usage:      r.Usage,
		Turns:      r.Turn,
		Repairs:    r.Repairs,
		ModelCalls: r.ModelCalls,
		ToolCalls:  r.ToolCalls,
		Duration:   time.Since(r.Started),
		Path:       append([]State(nil), m.path...),
		Handoff:    m.handoff,
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
