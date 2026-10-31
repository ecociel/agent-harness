package harness

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"slices"
	"sync/atomic"
	"time"

	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/tool"
)

// Defaults for zero-valued Agent limits.
const (
	DefaultMaxTurns   = 8
	DefaultMaxRepairs = 2
	DefaultLease      = 10 * time.Minute
)

// maxInlineBackoff is the longest retry delay a worker sleeps through. A longer
// one parks a durable run on a WaitTimer.
const maxInlineBackoff = 30 * time.Second

// machine is the shell around the decider. It has one job: ask next what to do,
// do it, and record what happened. It holds no state about the run except the
// fold, and it writes the fold in one place, record.
type machine struct {
	a       *Agent
	journal Journal
	durable bool
	hooks   hookSet
	run     Run
	err     error       // the original error of a failed run, for errors.Is
	sealed  atomic.Bool // a redactor failed: no more text leaves this worker
}

// Run advances the run named by in as far as it can go and returns where it
// stands. It is the single entry point for starting, resuming, signalling and
// cancelling. See Input.
//
// A run that ends on purpose or suspends returns with a nil error. A run that
// fails returns with the error. ErrDrained, ErrFenced, ErrStaleSignal and a journal
// error mean this worker stopped and the run itself is unharmed.
//
// A context stops the worker, not a durable run. Cancelling it, or passing its
// deadline, is treated as a crash: nothing terminal is recorded, the step in flight
// stays an open intent, and Run returns ErrDrained. Only Input.Cancel ends a durable
// run. A run with no Journal has no later, so for it a cancelled context records
// StopCancelled.
func (a *Agent) Run(ctx context.Context, in Input) (*Run, error) {
	if a.Provider == nil && a.composite == nil {
		return nil, errors.New("harness: agent has no Provider")
	}
	m := &machine{a: a, hooks: indexHooks(a.Hooks), journal: a.Journal, durable: a.Journal != nil}
	if inherited, ok := ctx.Value(keyRedactors).([]Redactor); ok {
		m.hooks.redact = slices.Concat(inherited, m.hooks.redact)
	}
	if m.journal == nil {
		if j, ok := ctx.Value(keyJournal).(Journal); ok {
			m.journal, m.durable = j, true
		} else {
			m.journal = NewMemJournal()
		}
	}
	id := in.RunID
	if id == "" {
		id = newRunID(a.Name)
	}
	evs, err := m.journal.Load(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("harness: load %s: %w: %w", id, ErrDrained, err)
	}
	r, err := Fold(evs)
	if err != nil {
		return nil, fmt.Errorf("harness: fold %s: %w", id, err)
	}
	m.run = *r
	switch {
	case m.run.State != StateStart:
		err = m.attach(ctx, in)
	case len(in.Messages) == 0:
		err = fmt.Errorf("%w: %s", ErrNoSuchRun, id)
	default:
		err = m.start(ctx, id, in)
	}
	if err != nil {
		return nil, err
	}
	if err := m.admit(ctx); err != nil {
		return nil, err
	}
	return m.loop(ctx)
}

// start records the run's identity, pin and input before anything else happens.
func (m *machine) start(ctx context.Context, id string, in Input) error {
	root := id
	p, nested := ctx.Value(keyParent).(parentInfo)
	if nested {
		root = p.root
	}
	return m.record(ctx, Event{Kind: KindRunStarted, RunID: id, RootRun: root, ParentRun: ParentRun(ctx),
		Agent: m.a.Name, Depth: Depth(ctx), Pin: m.a.pin(), Messages: in.Messages})
}

func (a *Agent) pin() *Pin {
	if a.composite != nil {
		return &Pin{Schema: schemaVersion, Shape: cmp.Or(a.composite.Shape, "composite")}
	}
	p := &Pin{Schema: schemaVersion, Instructions: a.Instructions, Model: a.Model, MaxTurns: a.MaxTurns,
		MaxRepairs: a.MaxRepairs, MaxTokens: a.MaxTokens, Temperature: a.Temperature,
		Tools: map[string]string{}}
	if p.MaxTurns <= 0 {
		p.MaxTurns = DefaultMaxTurns
	}
	if p.MaxRepairs <= 0 {
		p.MaxRepairs = DefaultMaxRepairs
	}
	for _, s := range a.Tools.Specs() {
		p.Tools[s.Name] = fingerprint(s)
	}
	return p
}

// attach takes an existing run. A terminal run, or a suspended run that nothing
// has answered, needs no write. Anything else claims the run, and the claim is the
// fence: the previous worker's next append takes a position that is now gone.
func (m *machine) attach(ctx context.Context, in Input) error {
	r := &m.run
	if r.State.Terminal() {
		return nil
	}
	now := time.Now()
	resolved, err := resolutions(r, in.Signals, now)
	if err != nil {
		return err
	}
	if r.State == StateSuspended && len(resolved) == 0 && in.Cancel == "" {
		return nil
	}
	drift, err := m.drift(ctx)
	if err != nil {
		return m.fail(ctx, err)
	}
	host, _ := os.Hostname()
	evs := []Event{{Kind: KindRunClaimed, Text: fmt.Sprintf("%s/%d", host, os.Getpid()), Drift: drift}}
	evs = append(evs, resolved...)
	if in.Cancel != "" && r.State != StateClosing {
		evs = append(evs, Event{Kind: KindRunStopping, Stop: StopCancelled, Text: in.Cancel})
	}
	return m.record(ctx, evs...)
}

// resolutions turns delivered signals and passed expiries into wait.resolved
// events. It writes nothing, so a rejected signal leaves the run untouched.
func resolutions(r *Run, signals []Signal, now time.Time) ([]Event, error) {
	var evs []Event
	expired := map[string]bool{}
	for _, w := range r.Waits {
		if !w.Expires.IsZero() && !now.Before(w.Expires) {
			expired[w.ID] = true
			evs = append(evs, Event{Kind: KindWaitResolved, Signal: &Signal{Wait: w.ID, Kind: w.Kind, Expired: true}})
		}
	}
	seen := map[string]bool{}
	for _, s := range signals {
		i := slices.IndexFunc(r.Waits, func(w Wait) bool { return w.ID == s.Wait })
		switch {
		case seen[s.Wait] || s.Expired:
			continue
		case i >= 0 && !expired[s.Wait]:
			s.Kind, s.Expired = r.Waits[i].Kind, false
			seen[s.Wait] = true
			evs = append(evs, Event{Kind: KindWaitResolved, Signal: &s})
		case !answered(r, s.Wait):
			return nil, fmt.Errorf("%w: %s", ErrStaleSignal, s.Wait)
		}
	}
	return evs, nil
}

// answered reports whether a wait already took a real signal, which makes another
// one a duplicate delivery. A wait that expired was not answered.
func answered(r *Run, id string) bool {
	return slices.ContainsFunc(r.Events, func(e Event) bool {
		return e.Kind == KindWaitResolved && e.Signal.Wait == id && !e.Signal.Expired
	})
}

// drift compares the live agent with the pin, on what the journal could hold.
func (m *machine) drift(ctx context.Context) (*Drift, error) {
	live := Event{Kind: KindRunStarted, Pin: m.a.pin()}
	if err := m.scrub(ctx, &live); err != nil {
		return nil, err
	}
	d := &Drift{Instructions: live.Pin.Instructions != m.run.Pin.Instructions, Shape: live.Pin.Shape != m.run.Pin.Shape}
	for name, fp := range m.run.Pin.Tools {
		if live.Pin.Tools[name] != fp {
			d.Tools = append(d.Tools, name)
		}
	}
	slices.Sort(d.Tools)
	return d, nil
}

// admit runs BeforeRun for a worker that is about to do work.
func (m *machine) admit(ctx context.Context) error {
	if s := m.run.State; s.Terminal() || s == StateSuspended || s == StateClosing {
		return nil
	}
	for _, h := range m.hooks.beforeRun {
		err := m.invoke(ctx, h, PointBeforeRun, func(ctx context.Context) (string, error) {
			return "ok", h.BeforeRun(ctx, m.view())
		})
		if err != nil {
			return m.fail(ctx, err)
		}
	}
	return nil
}

var errSealed = errors.New("harness: a redactor failed")

// loop is the whole machine: decide, do, record. Any error here is one the
// journal cannot absorb, so the worker stops as if it had crashed.
func (m *machine) loop(ctx context.Context) (*Run, error) {
	for {
		if err := m.interrupted(ctx); err != nil {
			return m.finish(err)
		}
		var err error
		switch s := next(&m.run).(type) {
		case stepCompose:
			err = m.compose(ctx)
		case stepLimit:
			err = m.record(ctx, Event{Kind: KindRunStopping, Stop: s.reason, Text: s.detail})
		case stepLoseModel:
			err = m.record(ctx, Event{Kind: KindModelLost, Attempt: m.run.attempt})
		case stepBody:
			err = m.body(ctx)
		case stepGate:
			err = m.gate(ctx, s.calls)
		case stepExecute:
			err = m.execute(ctx, s.calls)
		case stepLoseTools:
			evs := make([]Event, len(s.calls))
			for i, c := range s.calls {
				evs[i] = Event{Kind: KindToolLost, Index: c.Index, Tool: c.Name}
			}
			err = m.record(ctx, evs...)
		case stepVerify:
			err = m.verify(ctx, s.output)
		case stepClose:
			err = m.close(ctx)
		case stepPark:
			if m.durable {
				return m.finish(nil)
			}
			err = m.fail(ctx, ErrNotDurable)
		case stepEnd:
			return m.finish(nil)
		}
		if err != nil {
			return m.finish(err)
		}
	}
}

// interrupted reports whether this worker must stop before the next step.
func (m *machine) interrupted(ctx context.Context) error {
	s := m.run.State
	switch {
	case s.Terminal() || s == StateSuspended || s == StateClosing:
		return nil
	case m.durable && (draining(ctx) || ctx.Err() != nil):
		return ErrDrained
	case context.Cause(ctx) != nil:
		return m.record(ctx, Event{Kind: KindRunStopping, Stop: StopCancelled, Text: context.Cause(ctx).Error()})
	}
	return nil
}

func (m *machine) finish(err error) (*Run, error) {
	if err != nil {
		return nil, err
	}
	res := m.view()
	switch {
	case res.State != StateFailed:
		return &res, nil
	case m.err != nil:
		return &res, m.err
	}
	return &res, errors.New(res.Detail)
}

// record is the one way anything happens. In this order, always: redact, check
// the edge, append, adopt, emit. Nothing reaches the history, the journal, a hook
// or a sink except through here, so redaction, ordering and fencing each hold in
// one place. It ignores cancellation because an effect that happened must still
// be written down.
func (m *machine) record(ctx context.Context, evs ...Event) error {
	if len(evs) == 0 {
		return nil
	}
	probe := m.run
	probe.batch = slices.Clone(probe.batch)
	now := time.Now()
	first := len(probe.Events)
	for _, ev := range evs {
		ev.Seq, ev.Time = probe.seq+1, now
		if ev.Kind != KindRunStarted {
			ev.RunID, ev.RootRun, ev.ParentRun, ev.Agent, ev.Depth =
				probe.ID, probe.RootRun, probe.ParentRun, probe.Agent, probe.Depth
		}
		ev.Turn = probe.Turn
		for i := range ev.Waits {
			w := &ev.Waits[i]
			if w.ID == "" {
				w.ID, w.Run = waitID(ev.RunID, ev.Seq, i), ev.RunID
				if w.TTL > 0 {
					w.Expires = now.Add(w.TTL)
				}
			}
		}
		if err := m.scrub(ctx, &ev); err != nil {
			return m.fail(ctx, err)
		}
		if err := probe.apply(ev); err != nil {
			return err
		}
	}
	stored := probe.Events[first:]
	stored[len(stored)-1].Wake = probe.wake(now.Add(cmp.Or(m.a.Lease, DefaultLease)))
	if err := m.journal.Append(context.WithoutCancel(ctx), probe.ID, stored); err != nil {
		if errors.Is(err, ErrConflict) {
			return ErrFenced
		}
		return fmt.Errorf("harness: append %s: %w: %w", probe.ID, ErrDrained, err)
	}
	m.run = probe
	for _, ev := range stored {
		Emit(ctx, ev)
	}
	return nil
}

// fail ends the run as a failure. A hook's Stop ends it on purpose instead. A run
// that has not started has nothing to stop, and one already closing has decided
// its outcome, so either way the worker just stops as if it had crashed.
func (m *machine) fail(ctx context.Context, err error) error {
	if s := m.run.State; s == StateStart || s == StateClosing {
		return err
	}
	return m.record(ctx, m.stopEvent(err))
}

// stopEvent is the event that ends the run because of err.
func (m *machine) stopEvent(err error) Event {
	if s, ok := AsStop(err); ok {
		return Event{Kind: KindRunStopping, Stop: s.Reason, Text: s.Detail}
	}
	m.err = err
	f := &Failure{Class: model.ClassInternal, Detail: err.Error()}
	return Event{Kind: KindRunStopping, Stop: StopFailed, Text: f.Detail, Failure: f}
}

// scrub runs the redactors over one event. It fails closed: once a redactor has
// failed the worker is sealed, and the only text it still records is the fixed
// reason that ends the run.
func (m *machine) scrub(ctx context.Context, ev *Event) error {
	if len(m.hooks.redact) == 0 {
		return nil
	}
	if m.sealed.Load() {
		if ev.Kind != KindRunStopping && ev.Kind != KindRunEnded {
			ev.redact(func(s *string) { *s = "" })
			if ev.Failure != nil {
				ev.Failure.Detail = "harness: a redactor failed"
			}
		}
		return nil
	}
	var failed error
	ev.redact(func(s *string) {
		for _, h := range m.hooks.redact {
			if failed != nil {
				return
			}
			out, err := h.Redact(ctx, ev.Kind, *s)
			if err != nil {
				failed = fmt.Errorf("harness: redactor %s failed", h.Name())
				return
			}
			*s = out
		}
	})
	if failed == nil && ev.Reply != nil {
		for _, c := range ev.Reply.ToolCalls {
			if !json.Valid(c.Arguments) {
				failed = errors.New("harness: redaction left invalid tool arguments")
			}
		}
	}
	if failed != nil {
		m.sealed.Store(true)
	}
	return failed
}

// scrubText redacts one string outside any event.
func (m *machine) scrubText(ctx context.Context, kind Kind, text string) (string, error) {
	ev := Event{Kind: kind, Text: text}
	err := m.scrub(ctx, &ev)
	return ev.Text, err
}

// scrubbed is an error whose text was redacted. It answers errors.Is for its cause
// and has no Unwrap, because the cause still holds the raw text.
type scrubbed struct {
	text  string
	cause error
}

func (e *scrubbed) Error() string        { return e.text }
func (e *scrubbed) Is(target error) bool { return errors.Is(e.cause, target) }

// scrubError redacts what an error carries and keeps what the machine branches on.
func (m *machine) scrubError(ctx context.Context, kind Kind, err error) error {
	var (
		parked *suspension
		h      *tool.Handoff
		s      *StopError
		e      *model.Error
	)
	switch {
	case err == nil, len(m.hooks.redact) == 0, errors.Is(err, ErrFenced), errors.Is(err, ErrDrained), errors.As(err, &parked):
		return err // no text from outside, or text that passes record
	case errors.As(err, &h):
		note, rerr := m.scrubText(ctx, kind, h.Note)
		if rerr != nil {
			return rerr
		}
		return &tool.Handoff{Target: h.Target, Note: note}
	case errors.As(err, &s):
		detail, rerr := m.scrubText(ctx, kind, s.Detail)
		if rerr != nil {
			return rerr
		}
		return Stop(s.Reason, detail)
	case errors.As(err, &e):
		inner, rerr := m.scrubText(ctx, kind, e.Err.Error())
		if rerr != nil {
			return &model.Error{Class: model.ClassInternal, Usage: e.Usage, Err: rerr}
		}
		return &model.Error{Class: e.Class, RetryAfter: e.RetryAfter, Usage: e.Usage, Err: &scrubbed{inner, e.Err}}
	}
	text, rerr := m.scrubText(ctx, kind, err.Error())
	if rerr != nil {
		return rerr
	}
	return &scrubbed{text, err}
}

// ringModel is the innermost layer around the provider, so a retry, a fallback or
// any WrapModel hook meets only what the redactors left. A redactor cannot judge a
// fragment, so unless every redactor filters the stream itself the ring collects
// the reply, redacts it whole and streams the result. Failure discards the reply
// and keeps its usage, because the provider billed it.
func (m *machine) ringModel(v Run) model.Provider {
	if len(m.hooks.redact) == 0 {
		return m.a.Provider
	}
	p, blind := m.a.Provider, false
	for i := len(m.hooks.redact) - 1; i >= 0; i-- {
		if mw, ok := m.hooks.redact[i].(ModelMiddleware); ok {
			p = mw.WrapModel(v, p)
		} else {
			blind = true
		}
	}
	return model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
		return func(yield func(model.Event, error) bool) {
			if !blind {
				for e, err := range p.Complete(ctx, req) {
					if err != nil {
						yield(nil, m.scrubError(ctx, KindModelFailed, err))
						return
					}
					if !yield(e, nil) {
						return
					}
				}
				return
			}
			resp, err := model.Collect(p.Complete(ctx, req), nil)
			if err != nil {
				yield(nil, m.scrubError(ctx, KindModelFailed, err))
				return
			}
			ev := Event{Kind: KindModelDone, Reply: &resp}
			if err := m.scrub(ctx, &ev); err != nil {
				yield(nil, &model.Error{Class: model.ClassInternal, Usage: resp.Usage, Err: err})
				return
			}
			for e, err := range model.Stream(*ev.Reply) {
				if !yield(e, err) {
					return
				}
			}
		}
	})
}

// ringTool is the innermost layer around a tool, for the same reason. It runs on
// the tool's goroutine, which is why sealed is atomic.
func (m *machine) ringTool() ToolFunc {
	return func(ctx context.Context, c Call) (string, error) {
		out, err := m.a.Tools.Call(ctx, c.Name, c.Arguments)
		text, rerr := m.scrubText(ctx, KindToolDone, out)
		if rerr != nil {
			return "", rerr
		}
		return text, m.scrubError(ctx, KindToolDone, err)
	}
}

// view is the copy of the run a hook receives.
func (m *machine) view() Run {
	v := m.run
	v.Messages = slices.Clone(v.Messages)
	v.Waits = slices.Clone(v.Waits)
	v.batch = slices.Clone(v.batch)
	v.Events = v.Events[:len(v.Events):len(v.Events)]
	return v
}

// compose runs Context hooks, whose edits are recorded, then BeforeModel hooks,
// whose edits are not, then makes the model call.
func (m *machine) compose(ctx context.Context) error {
	if !m.run.retryAt.IsZero() {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Until(m.run.retryAt)):
		}
	}
	for _, h := range m.hooks.context {
		var e Edit
		err := m.invoke(ctx, h, PointContext, func(ctx context.Context) (string, error) {
			var err error
			e, err = h.Context(ctx, m.view())
			return "ok", err
		})
		if err != nil {
			return m.fail(ctx, err)
		}
		if e.zero() {
			continue
		}
		if err := checkEdit(m.run.Messages, e); err != nil {
			return m.fail(ctx, fmt.Errorf("hook %s: %w", h.Name(), err))
		}
		if err := m.record(ctx, Event{Kind: KindHistoryEdited, Hook: h.Name(), Edit: &e}); err != nil {
			return err
		}
	}
	req := m.request()
	for _, h := range m.hooks.beforeModel {
		err := m.invoke(ctx, h, PointBeforeModel, func(ctx context.Context) (string, error) {
			return "ok", h.BeforeModel(ctx, m.view(), &req)
		})
		if err != nil {
			return m.fail(ctx, err)
		}
	}
	// A BeforeModel hook may add text. What reaches WrapModel and the model is
	// held to the same rule as what reaches the history.
	ev := Event{Kind: KindModelStarted, Messages: req.Messages}
	if err := m.scrub(ctx, &ev); err != nil {
		return m.fail(ctx, err)
	}
	req.Messages = ev.Messages
	return m.callModel(ctx, req)
}

// checkEdit validates hook output at the boundary, so the fold can trust it.
func checkEdit(msgs []model.Message, e Edit) error {
	if e.From < 0 || e.To < e.From || e.To > len(msgs) {
		return fmt.Errorf("edit range [%d:%d] outside %d messages", e.From, e.To, len(msgs))
	}
	if !balanced(slices.Concat(msgs[:e.From:e.From], e.With, msgs[e.To:])) {
		return errors.New("edit separates a tool call from its result")
	}
	return nil
}

// balanced reports whether every tool result answers a call and every call is
// answered before the conversation moves on.
func balanced(msgs []model.Message) bool {
	open := map[string]bool{}
	for _, msg := range msgs {
		if msg.Role == model.RoleTool {
			if !open[msg.ToolCallID] {
				return false
			}
			delete(open, msg.ToolCallID)
			continue
		}
		if len(open) > 0 {
			return false
		}
		for _, c := range msg.ToolCalls {
			open[c.ID] = true
		}
	}
	return len(open) == 0
}

// request is pure over the fold and the live tool specs.
func (m *machine) request() model.Request {
	r := &m.run
	msgs := make([]model.Message, 0, len(r.Messages)+1)
	if r.Pin.Instructions != "" {
		msgs = append(msgs, model.System(r.Pin.Instructions))
	}
	msgs = append(msgs, r.Messages...)
	var specs []model.ToolSpec
	for _, s := range m.a.Tools.Specs() {
		specs = append(specs, s.ModelSpec())
	}
	name := r.Pin.Model
	if r.modelName != "" {
		name = r.modelName
	}
	return model.Request{Model: name, Messages: msgs, Tools: specs,
		MaxTokens: r.Pin.MaxTokens, Temperature: r.Pin.Temperature}
}

// callModel brackets the paid call with its intent and its outcome. Usage is
// recorded before anything can look at the reply.
func (m *machine) callModel(ctx context.Context, req model.Request) error {
	attempt := m.run.attempt + 1
	offered := make([]string, len(req.Tools))
	for i, t := range req.Tools {
		offered[i] = t.Name
	}
	b, _ := json.Marshal(req)
	if err := m.record(ctx, Event{Kind: KindModelStarted, Attempt: attempt, Offered: offered, Text: hash(string(b))}); err != nil {
		return err
	}
	if stopped, err := m.cancelledAbove(ctx); stopped || err != nil {
		return err
	}
	v := m.view()
	p := m.ringModel(v)
	for i := len(m.hooks.wrapModel) - 1; i >= 0; i-- {
		p = m.hooks.wrapModel[i].WrapModel(v, p)
	}
	start := time.Now()
	resp, err := model.Collect(p.Complete(ctx, req), m.deltaSink(ctx))
	if err != nil {
		return m.modelFailed(ctx, err, attempt, time.Since(start))
	}
	billed := resp.Usage
	for _, h := range m.hooks.afterModel {
		err := m.invoke(ctx, h, PointAfterModel, func(ctx context.Context) (string, error) {
			var err error
			resp, err = h.AfterModel(ctx, m.view(), resp)
			return "ok", err
		})
		if err == nil {
			err = checkReply(h, resp)
		}
		if err != nil {
			return m.modelFailed(ctx, &model.Error{Class: model.ClassInternal, Usage: billed, Err: err}, attempt, time.Since(start))
		}
	}
	resp.Usage = billed
	return m.record(ctx, Event{Kind: KindModelDone, Attempt: attempt, Reply: &resp, Duration: time.Since(start)})
}

// checkReply validates hook output at the boundary, so the journal can store it.
func checkReply(h Hook, resp model.Response) error {
	for _, c := range resp.ToolCalls {
		if !json.Valid(c.Arguments) {
			return fmt.Errorf("hook %s: tool call %s has invalid arguments", h.Name(), c.Name)
		}
	}
	return nil
}

// deltaSink streams text to sinks. It sits outside the ring, so a delta is
// redacted before it exists here.
func (m *machine) deltaSink(ctx context.Context) func(model.Event) {
	turn := m.run.Turn
	return func(ev model.Event) {
		if d, ok := ev.(model.TextDelta); ok {
			m.ephemeral(ctx, Event{Kind: KindDelta, Text: d.Text, Turn: turn})
		}
	}
}

func (m *machine) modelFailed(ctx context.Context, err error, attempt int, took time.Duration) error {
	if draining(ctx) || context.Cause(ctx) != nil {
		return m.record(ctx, Event{Kind: KindModelLost, Attempt: attempt})
	}
	e := model.AsError(err)
	f := Failure{Class: e.Class, Detail: err.Error(), Attempt: attempt, RetryAfter: e.RetryAfter, Usage: e.Usage}
	ev := Event{Kind: KindModelFailed, Attempt: attempt, Failure: &f, Duration: took, Text: f.Detail}
	if s, ok := AsStop(err); ok {
		ev.Stop, ev.Text = s.Reason, s.Detail
		return m.record(ctx, ev)
	}
	if m.sealed.Load() || e.Class == model.ClassInternal {
		m.err = err
		return m.record(ctx, ev)
	}
	var rec Recovery
	for _, h := range m.hooks.recover {
		_ = m.invoke(ctx, h, PointRecover, func(ctx context.Context) (string, error) {
			rec = h.Recover(ctx, m.view(), f)
			return "ok", nil
		})
		if rec.Retry {
			break
		}
	}
	if rec.Retry {
		delay := max(rec.After, f.RetryAfter)
		rec.After = delay
		if delay > maxInlineBackoff && m.durable {
			ev.Waits = []Wait{{Kind: WaitTimer, Call: -1, TTL: delay}}
		}
	}
	ev.Recovery = &rec
	return m.record(ctx, ev)
}

// body runs a composite's work and records how it came out. Nothing is recorded
// before it, because it has no effect of its own: its children record theirs, and a
// crash anywhere leaves the state where it was for the next worker to run it again.
func (m *machine) body(ctx context.Context) error {
	r := &m.run
	output, below, err := m.a.composite.Body(m.nest(ctx, *r),
		Input{RunID: r.ID, Messages: slices.Clone(r.Messages), Signals: slices.Clone(r.heard)})
	ev := Event{Kind: KindBodyDone, Reply: &model.Response{Text: output}, Below: &below}
	var parked *suspension
	switch {
	case err == nil:
	case errors.Is(err, ErrFenced) || errors.Is(err, ErrDrained):
		return err
	case errors.As(err, &parked):
		ev.Waits = slices.Clone(parked.waits)
	default:
		stop := m.stopEvent(err)
		ev.Stop, ev.Text, ev.Failure = cmp.Or(stop.Stop, StopFailed), stop.Text, stop.Failure
	}
	return m.record(ctx, ev)
}

// nest is the context a run hands to what it starts: a child takes its parent, its
// journal and its redactors from here.
func (m *machine) nest(ctx context.Context, v Run) context.Context {
	ctx = context.WithValue(ctx, keyParent, parentInfo{runID: v.ID, root: v.RootRun, depth: v.Depth})
	if m.durable {
		ctx = context.WithValue(ctx, keyJournal, m.journal)
	}
	return context.WithValue(ctx, keyRedactors, m.hooks.redact)
}

// gate decides each call and records all the decisions together.
func (m *machine) gate(ctx context.Context, calls []Call) error {
	evs := make([]Event, 0, len(calls))
	for _, c := range calls {
		if t, ok := m.a.Tools.Get(c.Name); ok {
			c.Spec = t.Spec()
		}
		ev := Event{Kind: KindToolGated, Index: c.Index, Tool: c.Name, Effect: c.Spec.Effect}
		d := Allow()
		for _, h := range m.hooks.beforeTool {
			_ = m.invoke(ctx, h, PointBeforeTool, func(ctx context.Context) (string, error) {
				d = h.BeforeTool(ctx, m.view(), c)
				return d.outcome(), nil
			})
			if d.Action != ActionAllow {
				ev.Hook = h.Name()
				break
			}
		}
		if d.Wait != nil {
			w := *d.Wait
			w.Call = c.Index
			ev.Waits = []Wait{w}
			d.Wait = nil
		}
		ev.Decision, ev.Outcome, ev.Text = &d, d.outcome(), d.Reason
		evs = append(evs, ev)
		if d.Action == ActionHalt {
			break
		}
	}
	return m.record(ctx, evs...)
}

// execute records the intent for a group atomically, runs the group, and records
// each result as it arrives. Tools only send on a channel: this goroutine stays
// the only writer, so a parallel group needs no locking around the fold.
func (m *machine) execute(ctx context.Context, calls []Call) error {
	started := make([]Event, len(calls))
	for i, c := range calls {
		started[i] = Event{Kind: KindToolStarted, Index: c.Index, Tool: c.Name, Attempt: c.attempt + 1}
	}
	if err := m.record(ctx, started...); err != nil {
		return err
	}
	if stopped, err := m.cancelledAbove(ctx); stopped || err != nil {
		return err
	}
	type outcome struct {
		ev  Event
		err error
	}
	results := make(chan outcome, len(calls))
	v := m.view()
	for _, c := range calls {
		go func() {
			ev, err := m.invoke1(ctx, v, c)
			results <- outcome{ev, err}
		}()
	}
	var first error
	for range calls {
		o := <-results
		switch {
		case first != nil:
		case m.run.State != StateTools:
			// An earlier result in this group ended the run and closed the batch.
		case o.err != nil && !isStop(o.err):
			first = o.err
		case m.durable && ctx.Err() != nil && o.ev.Outcome == "error":
			// The worker was stopped under the tool. That says nothing about the
			// effect, so the intent stays open for the next worker to judge.
		default:
			first = m.report(ctx, o.ev, o.err)
		}
	}
	return first
}

func isStop(err error) bool {
	_, ok := AsStop(err)
	return ok
}

// report records a finished call, revised by AfterTool hooks. The effect happened,
// so a call is recorded even when its result cannot be, and a stop lands in the same
// append as the call, so no crash can keep the fact and lose the decision.
func (m *machine) report(ctx context.Context, ev Event, stop error) error {
	if m.sealed.Load() {
		return m.record(ctx, ev, m.stopEvent(errSealed))
	}
	if ev.Outcome == "ok" || ev.Outcome == "error" {
		c := m.run.batch[ev.Index]
		if t, ok := m.a.Tools.Get(c.Name); ok {
			c.Spec = t.Spec()
		}
		for _, h := range m.hooks.afterTool {
			err := m.invoke(ctx, h, PointAfterTool, func(ctx context.Context) (string, error) {
				var err error
				ev.Text, err = h.AfterTool(ctx, m.view(), c, ev.Text)
				return "ok", err
			})
			if err != nil {
				ev.Outcome, ev.Text, ev.Hook, stop = "withheld", denied(c.Name, "withheld by "+h.Name()), h.Name(), err
				break
			}
		}
	}
	if stop != nil {
		return m.record(ctx, ev, m.stopEvent(stop))
	}
	return m.record(ctx, ev)
}

// invoke1 runs one call through its middleware. A fenced or drained child means
// this worker has stopped too, so that result must not be recorded.
func (m *machine) invoke1(ctx context.Context, v Run, c Call) (Event, error) {
	if t, ok := m.a.Tools.Get(c.Name); ok {
		c.Spec = t.Spec()
	}
	ctx = m.nest(ctx, v)
	ctx = context.WithValue(ctx, keySignals, c.toolSignals())
	ctx = tool.WithIdempotencyKey(ctx, c.Key)
	if c.Spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Spec.Timeout)
		defer cancel()
	}
	exec := m.ringTool()
	for i := len(m.hooks.wrapTool) - 1; i >= 0; i-- {
		exec = m.hooks.wrapTool[i].WrapTool(v, exec)
	}
	start := time.Now()
	content, err := exec(ctx, c)
	ev := Event{Kind: KindToolDone, Index: c.Index, Tool: c.Name, Duration: time.Since(start), Text: content}
	var parked *suspension
	switch {
	case errors.Is(err, ErrFenced) || errors.Is(err, ErrDrained):
		return ev, err
	case isStop(err):
		ev.Outcome = "stop"
		return ev, err
	case errors.As(err, &parked):
		ev.Outcome = "parked"
		ev.Waits = slices.Clone(parked.waits)
		for i := range ev.Waits {
			ev.Waits[i].Call = c.Index
		}
	case err != nil:
		if h, ok := tool.AsHandoff(err); ok {
			ev.Outcome, ev.Handoff, ev.Text = "handoff", h, fmt.Sprintf(`{"handoff":%q}`, h.Target)
			break
		}
		ev.Outcome, ev.Text = "error", "error: "+err.Error()
	default:
		ev.Outcome = "ok"
	}
	return ev, nil
}

// verify runs the output chain and records one verdict.
func (m *machine) verify(ctx context.Context, output string) error {
	var v Verdict
	ev := Event{Kind: KindOutputChecked}
	for _, h := range m.hooks.output {
		_ = m.invoke(ctx, h, PointCheckOutput, func(ctx context.Context) (string, error) {
			v = h.CheckOutput(ctx, m.view(), output)
			return string(v.Action), nil
		})
		if v.Action != VerdictAccept {
			ev.Hook = h.Name()
			break
		}
		if v.Rewrite != "" {
			output = v.Rewrite
		}
	}
	ev.Verdict, ev.Text, ev.Outcome = &v, output, string(v.Action)
	return m.record(ctx, ev)
}

// cancelledAbove stops this run if a run above it was cancelled, and reports whether
// it did. It runs after an intent is recorded and before the effect, so nothing whose
// intent lands after the cancel goes ahead. It needs no child ID, because every run
// knows its parent. That is how a cancel reaches a child no walk can find: one a tool
// started under a name of its own, or one that did not exist when the cancel went by.
// An ancestor that cannot be read stops the worker, because it may say to stop.
func (m *machine) cancelledAbove(ctx context.Context) (bool, error) {
	above, ok := ctx.Value(keyJournal).(Journal)
	if !ok {
		return false, nil // nothing above is durable, so nothing can cancel it from outside
	}
	for id := m.run.ParentRun; id != ""; {
		l, err := lineage(ctx, above, id)
		if err != nil {
			return false, fmt.Errorf("harness: load %s: %w: %w", id, ErrDrained, err)
		}
		if l.Cancelled {
			return true, m.record(ctx, Event{Kind: KindRunStopping, Stop: StopCancelled, Text: l.Reason})
		}
		id = l.Parent
	}
	return false, nil
}

// cancelBelow ends what a cancelled run leaves with no worker to read the cancel: a
// parked child, or one whose worker died. It goes through each child's own Runner, the
// way a signal does, so the child's hooks and journal are its own.
func (m *machine) cancelBelow(ctx context.Context) error {
	v := m.view()
	ctx = m.nest(ctx, v)
	if c := m.a.composite; c != nil {
		if c.Cancel == nil {
			return nil
		}
		return c.Cancel(ctx, v.ID, v.Detail)
	}
	for _, e := range v.Events {
		if e.Kind != KindToolStarted {
			continue
		}
		t, _ := m.a.Tools.Get(e.Tool)
		if c, ok := t.(ChildTool); ok {
			if err := c.CancelChild(tool.WithIdempotencyKey(ctx, callKey(v.ID, e.Turn, e.Index)), v.Detail); err != nil {
				return err
			}
		}
	}
	return nil
}

// close runs AfterRun, then marks the run ended. The order makes both this and the
// cancel below it at least once: a crash before the mark repeats them on the next
// attach.
func (m *machine) close(ctx context.Context) error {
	ctx = context.WithoutCancel(ctx)
	if m.run.Stop == StopCancelled {
		if err := m.cancelBelow(ctx); err != nil {
			return err
		}
	}
	for _, h := range m.hooks.afterRun {
		_ = m.invoke(ctx, h, PointAfterRun, func(ctx context.Context) (string, error) {
			h.AfterRun(ctx, m.view())
			return "ok", nil
		})
	}
	return m.record(ctx, Event{Kind: KindRunEnded, Outcome: string(m.run.Stop), Text: m.run.Detail})
}

// invoke runs one hook body and reports it as an ephemeral event.
func (m *machine) invoke(ctx context.Context, h Hook, p Point, fn func(context.Context) (string, error)) error {
	start := time.Now()
	var note string
	outcome, err := fn(context.WithValue(ctx, keyNote, &note))
	ev := Event{Kind: KindHook, Hook: h.Name(), Point: p, Outcome: outcome, Duration: time.Since(start),
		Text: note, Turn: m.run.Turn}
	if err != nil {
		ev.Outcome = "error"
		if s, ok := AsStop(err); ok {
			ev.Outcome, ev.Text = "stop", string(s.Reason)+": "+s.Detail
		}
	}
	if m.scrub(ctx, &ev) != nil {
		ev.Text = ""
	}
	m.ephemeral(ctx, ev)
	return err
}

// ephemeral sends an event that nothing depends on.
func (m *machine) ephemeral(ctx context.Context, ev Event) {
	r := &m.run
	ev.RunID, ev.RootRun, ev.ParentRun, ev.Agent, ev.Depth = r.ID, r.RootRun, r.ParentRun, r.Agent, r.Depth
	Emit(ctx, ev)
}
