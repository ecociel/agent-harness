package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/tool"
)

// schemaVersion is the event format this package writes and reads. Fields are
// only ever added. A breaking change bumps it, and a consumer's Journal.Load
// migrates older events.
const schemaVersion = 1

// ErrIllegalTransition is returned (wrapped) for an event the transition table
// does not allow in the current state.
var ErrIllegalTransition = errors.New("harness: illegal transition")

// Pin is what a run recorded at its start and keeps until it ends. Data is
// pinned; wiring (Provider, Tools, Hooks, Journal) is always live.
type Pin struct {
	Schema       int               `json:"schema"`
	Instructions string            `json:"instructions,omitempty"`
	Shape        string            `json:"shape,omitempty"` // what a composite run runs; empty for an agent
	Model        string            `json:"model,omitempty"`
	MaxTurns     int               `json:"max_turns"`
	MaxRepairs   int               `json:"max_repairs"`
	MaxTokens    int               `json:"max_tokens,omitempty"`
	Temperature  *float64          `json:"temperature,omitempty"`
	Tools        map[string]string `json:"tools,omitempty"` // name to spec fingerprint
}

// Drift is how the live agent differs from a run's Pin, found when a worker
// claims the run.
type Drift struct {
	Instructions bool     `json:"instructions,omitempty"`
	Tools        []string `json:"tools,omitempty"` // changed or removed since the pin
	// Shape means the run's work changed: a composite's parts were reordered, or the
	// run belongs to something else entirely. The run fails rather than hand one
	// part's journal to another.
	Shape bool `json:"shape,omitempty"`
}

// Spend is what model and tool calls added up to.
type Spend struct {
	Usage      model.Usage `json:"usage"`
	Turns      int         `json:"turns"`
	ModelCalls int         `json:"model_calls"`
	LostCalls  int         `json:"lost_calls,omitempty"`
	ToolCalls  int         `json:"tool_calls"`
}

// Add returns s plus o.
func (s Spend) Add(o Spend) Spend {
	return Spend{Usage: s.Usage.Add(o.Usage), Turns: s.Turns + o.Turns, ModelCalls: s.ModelCalls + o.ModelCalls,
		LostCalls: s.LostCalls + o.LostCalls, ToolCalls: s.ToolCalls + o.ToolCalls}
}

// callStatus is where one tool call of a batch stands.
type callStatus int

const (
	callPending  callStatus = iota // needs a decision
	callAllowed                    // decided, not started
	callAwaiting                   // waiting on a signal
	callStarted                    // intent recorded, no outcome
	callDone                       // content is the result for the model
)

// Call is one tool call as a hook sees it.
type Call struct {
	model.ToolCall
	Index   int      // position in the batch
	Key     string   // idempotency key: same across retries and resumes
	Signals []Signal // what the hooks' waits for this call have received
	// Spec is the live tool metadata, filled in for hooks. It is never recorded.
	Spec tool.Spec

	status  callStatus
	effect  tool.Effect // recorded at the gate, so a lost call keeps the class it ran under
	attempt int
	content string
	parked  []string // waits the tool itself opened
}

// toolSignals is what a re-invoked tool may see: answers to the waits it last parked
// on. A gate hook's signals belong to the hook, so a sub-agent tool cannot forward a
// wait its child has never heard of, and an answer to an earlier ask stays behind, so
// it cannot make a child refuse the answer that followed it.
func (c Call) toolSignals() []Signal {
	var out []Signal
	for _, s := range c.Signals {
		if slices.Contains(c.parked, s.Wait) {
			out = append(out, s)
		}
	}
	return out
}

// Signal returns the latest signal of the given wait kind for this call.
func (c Call) Signal(kind string) (Signal, bool) {
	for i := len(c.Signals) - 1; i >= 0; i-- {
		if c.Signals[i].Kind == kind {
			return c.Signals[i], true
		}
	}
	return Signal{}, false
}

// Run is everything known about a run: computed by Fold and written by nothing
// else. It is what a caller gets from Agent.Run and what a hook gets by value, so
// a reader of the journal, a hook and a dashboard all hold the same thing. If
// State is not terminal the run is not over: Stop says why Run returned, and Waits
// says what it needs.
type Run struct {
	ID        string `json:"run_id"`
	RootRun   string `json:"root_run,omitempty"`
	ParentRun string `json:"parent_run,omitempty"`
	Agent     string `json:"agent"`
	Depth     int    `json:"depth"`
	Pin       Pin    `json:"pin"`
	State     State  `json:"state"`
	// Path is the sequence of states visited, Start first.
	Path []State `json:"path"`
	// Stop, Detail and Class say how the run ended, or why it paused.
	Stop   StopReason       `json:"stop"`
	Detail string           `json:"detail,omitempty"`
	Class  model.ErrorClass `json:"class,omitempty"`
	Output string           `json:"output"`
	// Messages is the conversation without the system instructions. While a batch
	// is open its results are held back, and they join in call order when it
	// closes. So every tool call here has a result at each model call and at the
	// end of the run, and a finished run's Messages can start the next run.
	Messages []model.Message `json:"messages"`
	Usage    model.Usage     `json:"usage"`
	Turn     int             `json:"turns"` // model turns begun
	Repairs  int             `json:"repairs"`
	// ModelCalls and ToolCalls count this run only, so a sum over the runs of a tree
	// counts each call once. LostCalls are model calls a crash or a cancel cut off,
	// billed or not, so what the provider charged lies between ModelCalls and
	// ModelCalls+LostCalls calls. Spent adds what a composite run's children reported.
	ModelCalls int `json:"model_calls"`
	LostCalls  int `json:"lost_calls,omitempty"`
	ToolCalls  int `json:"tool_calls"`
	Claims     int `json:"claims"` // how many workers have taken this run
	// Failure is the last failed attempt at the current turn.
	Failure *Failure      `json:"failure,omitempty"`
	Waits   []Wait        `json:"waits,omitempty"` // open waits
	Drift   Drift         `json:"drift"`
	Started time.Time     `json:"started"`
	Updated time.Time     `json:"updated"`
	Waited  time.Duration `json:"waited_ns"` // total time spent suspended
	Handoff *tool.Handoff `json:"handoff,omitempty"`
	// Events is the journal so far, for hooks that derive their own state.
	Events []Event `json:"-"`

	seq       int64
	attempt   int
	last      model.Response
	batch     []Call
	below     Spend    // what a composite's children had spent when it last reported
	heard     []Signal // answers to the waits a composite last parked on
	offered   []string
	retryAt   time.Time
	modelName string // a recovery's replacement, until the next reply
	parkedAt  time.Time
}

// OK reports whether the run completed with an accepted answer.
func (r *Run) OK() bool { return r != nil && r.Stop == StopCompleted }

// Duration is the time from start to the last recorded event.
func (r *Run) Duration() time.Duration { return r.Updated.Sub(r.Started) }

// Spent is this run's own spend plus what its body last reported for the runs below
// it, which is all a composite run knows. It is a report, not a projection: an agent
// cannot see its tools' children, and a run ended from outside reports nothing more.
// For a tree, sum the runs' own counters.
func (r *Run) Spent() Spend {
	return r.below.Add(Spend{Usage: r.Usage, Turns: r.Turn, ModelCalls: r.ModelCalls,
		LostCalls: r.LostCalls, ToolCalls: r.ToolCalls})
}

// Fold computes a run from its journal. It reads no clock, calls no hook and does
// no I/O. Every question about a run has one answer, and this is where it comes
// from: the live loop, a resume on another machine, a dashboard and an eval all
// go through the same apply.
//
// The journal is a boundary: a damaged log is an error here, and nothing past
// this function needs to doubt an event.
func Fold(events []Event) (r *Run, err error) {
	defer func() {
		if p := recover(); p != nil {
			r, err = nil, fmt.Errorf("%w: damaged journal: %v", ErrIllegalTransition, p)
		}
	}()
	r = &Run{State: StateStart, Path: []State{StateStart}}
	for _, ev := range events {
		if err := r.apply(ev); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// apply folds one event. All state changes in the package are in this function.
func (r *Run) apply(ev Event) error {
	if ev.Seq != r.seq+1 {
		return fmt.Errorf("%w: event %d after %d", ErrIllegalTransition, ev.Seq, r.seq)
	}
	from := r.State
	switch ev.Kind {
	case KindRunStarted:
		if ev.Pin == nil || ev.Pin.Schema > schemaVersion {
			return fmt.Errorf("%w: run %s has an unknown schema", ErrIllegalTransition, ev.RunID)
		}
		r.ID, r.RootRun, r.ParentRun, r.Agent, r.Depth = ev.RunID, ev.RootRun, ev.ParentRun, ev.Agent, ev.Depth
		r.Pin, r.Started = *ev.Pin, ev.Time
		r.Messages = slices.Clone(ev.Messages)
		r.State = StateCompose
		if r.composite() {
			r.State = StateBody
		}

	case KindRunClaimed:
		r.claim(ev)

	case KindHistoryEdited:
		r.edit(*ev.Edit)

	case KindModelStarted:
		if ev.Attempt == 1 {
			r.Turn++
		}
		r.attempt, r.offered, r.retryAt = ev.Attempt, ev.Offered, time.Time{}
		r.State = StateModel

	case KindModelDone:
		r.route(*ev.Reply)

	case KindModelFailed:
		r.Usage = r.Usage.Add(ev.Failure.Usage)
		r.Failure = ev.Failure
		switch {
		case ev.Stop != "":
			r.stop(ev.Stop, "", ev.Text)
		case ev.Recovery == nil || !ev.Recovery.Retry:
			r.stop(StopFailed, ev.Failure.Class, ev.Failure.Detail)
		case len(ev.Waits) > 0:
			r.modelName = ev.Recovery.Model
			r.open(ev.Waits, ev.Time)
		default:
			r.modelName, r.retryAt = ev.Recovery.Model, ev.Time.Add(ev.Recovery.After)
			r.State = StateCompose
		}

	case KindModelLost:
		r.LostCalls++
		r.State = StateCompose

	case KindToolGated:
		c := &r.batch[ev.Index]
		c.effect = ev.Effect
		switch ev.Decision.Action {
		case ActionAllow:
			c.status = callAllowed
		case ActionDeny:
			c.finish(denied(c.Name, ev.Decision.Reason))
		case ActionHalt:
			c.finish(denied(c.Name, ev.Decision.Reason))
			reason := ev.Decision.Stop
			if reason == "" {
				reason = StopGuardrail
			}
			r.stop(reason, "", ev.Decision.Reason)
			return r.commit(from, ev)
		case ActionAwait:
			c.status = callAwaiting
			r.addWaits(ev.Waits)
		}
		r.settle(ev.Time)

	case KindToolStarted:
		c := &r.batch[ev.Index]
		c.status, c.attempt = callStarted, ev.Attempt

	case KindToolDone:
		c := &r.batch[ev.Index]
		switch {
		case ev.Handoff != nil:
			c.finish(ev.Text)
			r.Handoff = ev.Handoff
			r.stop(StopHandoff, "", "to "+ev.Handoff.Target)
			return r.commit(from, ev)
		case len(ev.Waits) > 0:
			c.status, c.parked = callAwaiting, nil
			for _, w := range ev.Waits {
				c.parked = append(c.parked, w.ID)
			}
			r.addWaits(ev.Waits)
		default:
			c.finish(ev.Text)
		}
		r.settle(ev.Time)

	case KindToolLost:
		c := &r.batch[ev.Index]
		if c.effect == tool.EffectOnce {
			c.status = callAwaiting
			r.addWaits([]Wait{{ID: waitID(r.ID, ev.Seq, ev.Index), Run: r.ID, Call: ev.Index,
				Kind: WaitUnknownOutcome, Detail: "the outcome of " + c.Name + " is unknown"}})
		} else {
			r.redecide(c, "the tool changed while this call was in flight, so its outcome is unknown")
		}
		r.settle(ev.Time)

	case KindBodyDone:
		r.bodyDone(ev)

	case KindWaitResolved:
		r.resolve(*ev.Signal, ev.Time)

	case KindOutputChecked:
		r.check(*ev.Verdict, ev.Text)

	case KindRunStopping:
		if r.State == StateModel {
			r.LostCalls++ // a cancel cut it off: it may be billed, and its reply is gone
		}
		r.stop(ev.Stop, failureClass(ev), ev.Text)

	case KindRunEnded:
		switch r.Stop {
		case StopCompleted:
			r.State = StateDone
		case StopFailed:
			r.State = StateFailed
		default:
			r.State = StateStopped
		}

	default:
		return fmt.Errorf("%w: unknown event %q", ErrIllegalTransition, ev.Kind)
	}
	return r.commit(from, ev)
}

func failureClass(ev Event) model.ErrorClass {
	if ev.Failure != nil {
		return ev.Failure.Class
	}
	return ""
}

// commit checks the edge an event took against the table.
func (r *Run) commit(from State, ev Event) error {
	if !CanTransition(from, ev.Kind, r.State) {
		return fmt.Errorf("%w: %s --%s--> %s", ErrIllegalTransition, from, ev.Kind, r.State)
	}
	ev.From, ev.To = from, r.State
	r.seq, r.Updated = ev.Seq, ev.Time
	r.Events = append(r.Events, ev)
	if r.State != from {
		r.Path = append(r.Path, r.State)
		if from == StateSuspended {
			r.Waited += ev.Time.Sub(r.parkedAt)
			if r.Stop == StopSuspended {
				r.Stop = ""
			}
		}
		if r.State == StateSuspended {
			r.parkedAt, r.Stop = ev.Time, StopSuspended
		}
	}
	return nil
}

// composite reports whether this run's work is a body, not a model and tools.
func (r *Run) composite() bool { return r.Pin.Shape != "" }

// bodyDone takes a body's report. What it spent below replaces the last report,
// because a body walks all its children each time. A run that parks starts hearing
// answers afresh, so one to an earlier ask cannot reach a child that has moved on.
func (r *Run) bodyDone(ev Event) {
	r.below = *ev.Below
	if len(ev.Waits) > 0 {
		r.heard = nil
		r.open(ev.Waits, ev.Time)
		return
	}
	r.last = *ev.Reply
	r.Messages = append(r.Messages, ev.Reply.Message())
	if ev.Stop == "" {
		r.State = StateVerify
		return
	}
	r.Output = ev.Reply.Text
	r.stop(ev.Stop, failureClass(ev), ev.Text)
}

// claim fences the previous worker and voids decisions about calls that have not
// started: whatever comes next happens in a world those decisions did not see.
func (r *Run) claim(ev Event) {
	r.Claims++
	if ev.Drift != nil {
		r.Drift = *ev.Drift
	}
	for i := range r.batch {
		if c := &r.batch[i]; c.status == callAllowed || c.status == callPending {
			r.redecide(c, plannedOnOldTool)
		}
	}
	if r.State == StateGate || r.State == StateTools {
		r.settle(ev.Time)
	}
}

const plannedOnOldTool = "the tool changed after this call was planned"

// redecide is the one way a call goes back to needing a decision, so no route
// around it can carry a call to a tool the run no longer knows.
func (r *Run) redecide(c *Call, reason string) {
	if slices.Contains(r.Drift.Tools, c.Name) {
		c.finish(denied(c.Name, reason))
		return
	}
	c.status = callPending
}

// edit applies one recorded history edit.
func (r *Run) edit(e Edit) {
	with := slices.Clone(e.With)
	for i := range with {
		with[i].Source = e.Source
	}
	r.Messages = slices.Concat(r.Messages[:e.From:e.From], with, r.Messages[e.To:])
}

// route turns a reply into the next state. It is the whole of what the finish
// reason means, and it is pure.
func (r *Run) route(resp model.Response) {
	r.Usage = r.Usage.Add(resp.Usage)
	r.ModelCalls++
	r.last, r.attempt, r.Failure, r.modelName = resp, 0, nil, ""
	msg := resp.Message()
	switch {
	case resp.Finish == model.FinishRefusal || resp.Finish == model.FinishContentFilter:
		msg.ToolCalls = nil
		r.Messages = append(r.Messages, msg)
		r.stop(StopRefused, "", string(resp.Finish))
		return
	case len(resp.ToolCalls) == 0 && resp.Finish == model.FinishLength:
		r.Messages = append(r.Messages, msg)
		r.Output = resp.Text
		r.stop(StopTruncated, "", "the reply hit the output limit")
		return
	case len(resp.ToolCalls) == 0:
		r.Messages = append(r.Messages, msg)
		r.State = StateVerify
		return
	}
	r.Messages = append(r.Messages, msg)
	r.batch = make([]Call, len(resp.ToolCalls))
	for i, tc := range resp.ToolCalls {
		c := Call{ToolCall: tc, Index: i, Key: callKey(r.ID, r.Turn, i)}
		switch {
		case resp.Finish == model.FinishLength:
			c.finish(denied(tc.Name, "the arguments were cut off by the output limit"))
		case !slices.Contains(r.offered, tc.Name):
			c.finish(denied(tc.Name, "this tool was not offered"))
		}
		r.batch[i] = c
	}
	r.settle(time.Time{})
}

// settle picks the state a batch is in. A batch moves in one order: every call
// decided, then every wait resolved, then every allowed call run, then the batch
// closes. Started calls hold it in tools until they report or are declared lost.
func (r *Run) settle(now time.Time) {
	if r.composite() {
		r.State = StateBody
		return
	}
	var started, pending, allowed bool
	for _, c := range r.batch {
		started = started || c.status == callStarted
		pending = pending || c.status == callPending
		allowed = allowed || c.status == callAllowed
	}
	switch {
	case started:
		r.State = StateTools
	case pending:
		r.State = StateGate
	case len(r.Waits) > 0:
		r.State = StateSuspended
	case allowed:
		r.State = StateTools
	default:
		r.closeBatch("")
		r.State = StateCompose
	}
}

// closeBatch moves a batch's results into the history in call order. Every call
// gets exactly one result, even the ones that never ran. This is what keeps a
// transcript valid for the provider on the next turn, whatever ended the batch. A
// started call may have happened, so the model is not told it was skipped: a
// transcript can start another run, and a model told that would do it again.
func (r *Run) closeBatch(reason string) {
	for _, c := range r.batch {
		content := c.content
		switch c.status {
		case callDone:
			r.ToolCalls++
		case callStarted:
			content = fmt.Sprintf(`{"unknown":true,"tool":%q,"reason":%q}`, c.Name, reason)
		default:
			content = fmt.Sprintf(`{"skipped":true,"tool":%q,"reason":%q}`, c.Name, reason)
		}
		r.Messages = append(r.Messages, model.ToolResult(c.ID, c.Name, content))
	}
	r.batch = nil
}

// stop decides the outcome. The batch closes, and open waits no longer matter.
func (r *Run) stop(reason StopReason, class model.ErrorClass, detail string) {
	r.Stop, r.Class, r.Detail = reason, class, detail
	r.closeBatch(string(reason))
	r.Waits = nil
	r.State = StateClosing
}

func (r *Run) open(waits []Wait, now time.Time) {
	r.addWaits(waits)
	r.State = StateSuspended
}

func (r *Run) addWaits(waits []Wait) { r.Waits = append(r.Waits, waits...) }

// resolve closes one wait. The unknown-outcome wait is the one kind whose signal
// IS the result, because the call must not run again. Any other wait sends its
// call back to be decided, with the signal visible to the hooks.
func (r *Run) resolve(s Signal, now time.Time) {
	i := slices.IndexFunc(r.Waits, func(w Wait) bool { return w.ID == s.Wait })
	if i < 0 {
		return
	}
	w := r.Waits[i]
	r.Waits = slices.Delete(slices.Clone(r.Waits), i, i+1)
	if r.composite() {
		r.heard = append(r.heard, s)
	} else if w.Call >= 0 && w.Call < len(r.batch) {
		c := &r.batch[w.Call]
		c.Signals = append(c.Signals, s)
		if w.Kind == WaitUnknownOutcome && w.Run == r.ID {
			c.finish(s.Value)
		} else {
			r.redecide(c, plannedOnOldTool)
		}
	}
	if len(r.Waits) > 0 {
		return
	}
	r.settle(now)
}

// check applies a verdict.
func (r *Run) check(v Verdict, output string) {
	switch v.Action {
	case VerdictBlock:
		r.stop(StopGuardrail, "", v.Feedback)
	case VerdictRevise:
		if r.Repairs >= r.Pin.MaxRepairs {
			r.Output = output
			r.stop(StopMaxRepairs, "", v.Feedback)
			return
		}
		r.Repairs++
		msg := model.User("Your previous answer was rejected by a check: " + v.Feedback + "\nPlease revise your answer.")
		msg.Source = "verify"
		r.Messages = append(r.Messages, msg)
		r.State = StateCompose
	default:
		r.Output = output
		if n := len(r.Messages); n > 0 {
			r.Messages[n-1].Content = output
		}
		r.stop(StopCompleted, "", "")
	}
}

// wake is when this run next needs a worker, given the end of the current worker's
// lease. Zero means nobody needs to call Run: it is over, or only a signal moves it.
func (r *Run) wake(lease time.Time) time.Time {
	switch {
	case r.State.Terminal():
		return time.Time{}
	case r.composite() && r.ParentRun != "":
		return time.Time{} // never swept, and no sign of life: only its children work
	case r.State != StateSuspended:
		return lease
	}
	var at time.Time
	for _, w := range r.Waits {
		if !w.Expires.IsZero() && (at.IsZero() || w.Expires.Before(at)) {
			at = w.Expires
		}
	}
	return at
}

func (c *Call) finish(content string) { c.status, c.content = callDone, content }

func denied(name, reason string) string {
	return fmt.Sprintf(`{"denied":true,"tool":%q,"reason":%q}`, name, reason)
}

// step is what the shell must do next. There is one step per live state, so the
// shell has nothing to decide. Each step ends by recording events.
type step interface{ isStep() }

type (
	stepCompose struct{} // Context, BeforeModel, then the model call
	stepLimit   struct {
		reason StopReason
		detail string
	} // stop on a pinned limit
	stepLoseModel struct{}                // a started model call has no outcome
	stepBody      struct{}                // a composite's work
	stepGate      struct{ calls []Call }  // BeforeTool on each
	stepExecute   struct{ calls []Call }  // run these together
	stepLoseTools struct{ calls []Call }  // started calls have no outcome
	stepVerify    struct{ output string } // CheckOutput
	stepClose     struct{}                // AfterRun, then run.ended
	stepPark      struct{}                // return to the caller: waits are open
	stepEnd       struct{}                // return to the caller: terminal
)

func (stepCompose) isStep()   {}
func (stepLimit) isStep()     {}
func (stepLoseModel) isStep() {}
func (stepBody) isStep()      {}
func (stepGate) isStep()      {}
func (stepExecute) isStep()   {}
func (stepLoseTools) isStep() {}
func (stepVerify) isStep()    {}
func (stepClose) isStep()     {}
func (stepPark) isStep()      {}
func (stepEnd) isStep()       {}

// next is the decider: a pure function from a fold to the next step. It mirrors
// corporate-admin's agent.Next(limits, turn), with the limits pinned in the run.
func next(r *Run) step {
	if r.Drift.Shape && r.State.Loop() == "inner" {
		return stepLimit{StopFailed, "the parts of this run changed after it started"}
	}
	switch r.State {
	case StateBody:
		return stepBody{}
	case StateCompose:
		if r.attempt == 0 && r.Turn >= r.Pin.MaxTurns {
			return stepLimit{StopMaxTurns, fmt.Sprintf("reached %d turns", r.Pin.MaxTurns)}
		}
		return stepCompose{}
	case StateModel:
		return stepLoseModel{}
	case StateGate:
		return stepGate{r.calls(callPending)}
	case StateTools:
		if lost := r.calls(callStarted); len(lost) > 0 {
			return stepLoseTools{lost}
		}
		return stepExecute{r.nextGroup()}
	case StateVerify:
		return stepVerify{r.last.Text}
	case StateSuspended:
		return stepPark{}
	case StateClosing:
		return stepClose{}
	}
	return stepEnd{}
}

func (r *Run) calls(s callStatus) []Call {
	var out []Call
	for _, c := range r.batch {
		if c.status == s {
			out = append(out, c)
		}
	}
	return out
}

// nextGroup is the parallelism policy: adjacent read calls run together, and any
// other call runs alone. Model order holds across groups.
func (r *Run) nextGroup() []Call {
	allowed := r.calls(callAllowed)
	if allowed[0].effect != tool.EffectRead {
		return allowed[:1]
	}
	n := 1
	for n < len(allowed) && allowed[n].effect == tool.EffectRead && allowed[n].Index == allowed[n-1].Index+1 {
		n++
	}
	return allowed[:n]
}

// callKey is the idempotency key of a call. The turn and index come from the
// journal, so every retry and resume derives the same key, and the provider's own
// call id, which can repeat across turns, plays no part.
func callKey(runID string, turn, index int) string {
	return hash(fmt.Sprintf("%s\x00%d\x00%d", runID, turn, index))
}

// waitID is unique per opening: a wait opened again gets a new ID, so an answer to
// the earlier ask cannot satisfy it.
func waitID(runID string, seq int64, index int) string {
	return "w_" + hash(fmt.Sprintf("%s\x00%d\x00%d", runID, seq, index))
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:12])
}

func fingerprint(s tool.Spec) string {
	b, _ := json.Marshal(struct {
		D string
		P json.RawMessage
		R tool.Risk
		S string
		E tool.Effect
	}{s.Description, s.Parameters, s.Risk, s.Scope, s.Effect})
	return hash(string(b))
}
