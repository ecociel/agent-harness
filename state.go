package harness

// State is one node of the run state machine. A State is never stored: it is
// what Fold computes from the journal. The legal edges are the table below and
// nothing else, and both the live run and every replay are checked against it,
// so a journal cannot contain a walk the machine would refuse.
type State string

// The states. Compose, Model, Gate and Tools form the inner loop: one model/tool
// turn, repeated until the model answers without asking for tools.
const (
	// StateStart is an empty journal.
	StateStart State = "start"
	// StateCompose assembles context: Context hooks (durable), then BeforeModel
	// hooks (this call only).
	StateCompose State = "compose"
	// StateModel means a model call was recorded as started and has no outcome.
	// In a live run it lasts for the call. Found on resume, the call is lost.
	StateModel State = "model"
	// StateGate decides the calls of a batch: BeforeTool hooks. No call in a
	// batch starts until every call is decided.
	StateGate State = "gate"
	// StateTools runs allowed calls. Found on resume with a started call, that
	// call's outcome is unknown.
	StateTools State = "tools"
	// StateVerify runs CheckOutput hooks on the candidate answer.
	StateVerify State = "verify"
	// StateSuspended holds open waits. No worker is needed: Run returns, and a
	// later Run with a signal or past an expiry carries on.
	StateSuspended State = "suspended"
	// StateClosing means the outcome is decided and AfterRun hooks are due.
	StateClosing State = "closing"
	// StateDone ends a run that produced an accepted answer.
	StateDone State = "done"
	// StateStopped ends a run that a hook, a limit, a handoff or cancellation
	// ended on purpose.
	StateStopped State = "stopped"
	// StateFailed ends a run that hit an unrecoverable error.
	StateFailed State = "failed"
)

// Loop names which loop a state belongs to: "outer", "inner" or "terminal".
func (s State) Loop() string {
	switch s {
	case StateCompose, StateModel, StateGate, StateTools:
		return "inner"
	case StateDone, StateStopped, StateFailed:
		return "terminal"
	}
	return "outer"
}

// Terminal reports whether s ends the run.
func (s State) Terminal() bool { return s.Loop() == "terminal" }

// Edge is one legal step: in state From, recording On leads to To.
type Edge struct {
	From State `json:"from"`
	On   Kind  `json:"on"`
	To   State `json:"to"`
}

// edges is the specification of the machine. Every live state also takes
// run.claimed and run.stopping; init adds those so this table shows the flow.
var edges = []Edge{
	{StateStart, KindRunStarted, StateCompose},

	{StateCompose, KindHistoryEdited, StateCompose},
	{StateCompose, KindModelStarted, StateModel},

	{StateModel, KindModelDone, StateGate},    // tool calls
	{StateModel, KindModelDone, StateVerify},  // plain answer
	{StateModel, KindModelDone, StateCompose}, // no call survived routing
	{StateModel, KindModelDone, StateClosing}, // refused, or truncated answer
	{StateModel, KindModelFailed, StateCompose},
	{StateModel, KindModelFailed, StateSuspended}, // long backoff
	{StateModel, KindModelFailed, StateClosing},   // gave up
	{StateModel, KindModelLost, StateCompose},

	{StateGate, KindToolGated, StateGate},
	{StateGate, KindToolGated, StateTools},
	{StateGate, KindToolGated, StateSuspended},
	{StateGate, KindToolGated, StateCompose}, // every call denied
	{StateGate, KindToolGated, StateClosing}, // halt

	{StateTools, KindToolStarted, StateTools},
	{StateTools, KindToolDone, StateTools},
	{StateTools, KindToolDone, StateCompose},
	{StateTools, KindToolDone, StateSuspended}, // a tool parked
	{StateTools, KindToolDone, StateClosing},   // handoff
	{StateTools, KindToolLost, StateTools},     // more lost calls in this append
	{StateTools, KindToolLost, StateGate},      // repeatable: decide it again
	{StateTools, KindToolLost, StateCompose},   // a drifted tool emptied the batch
	{StateTools, KindToolLost, StateSuspended}, // EffectOnce: ask

	{StateSuspended, KindWaitResolved, StateSuspended},
	{StateSuspended, KindWaitResolved, StateGate},
	{StateSuspended, KindWaitResolved, StateCompose},

	{StateVerify, KindOutputChecked, StateCompose}, // revise
	{StateVerify, KindOutputChecked, StateClosing},

	{StateClosing, KindRunEnded, StateDone},
	{StateClosing, KindRunEnded, StateStopped},
	{StateClosing, KindRunEnded, StateFailed},

	{StateTools, KindRunClaimed, StateGate},    // nothing unstarted survives a claim undecided
	{StateTools, KindRunClaimed, StateCompose}, // a drifted tool emptied the batch
	{StateGate, KindRunClaimed, StateCompose},
}

var liveStates = []State{StateCompose, StateModel, StateGate, StateTools, StateVerify,
	StateSuspended, StateClosing}

var legal = map[Edge]bool{}

func init() {
	for _, s := range liveStates {
		edges = append(edges, Edge{s, KindRunClaimed, s})
		if s != StateClosing {
			edges = append(edges, Edge{s, KindRunStopping, StateClosing})
		}
	}
	for _, e := range edges {
		legal[e] = true
	}
}

// CanTransition reports whether recording on in state from may lead to to.
func CanTransition(from State, on Kind, to State) bool { return legal[Edge{from, on, to}] }

// Transitions returns a copy of the table, for documentation and diagrams.
func Transitions() []Edge { return append([]Edge(nil), edges...) }

// States lists every state in diagram order.
func States() []State {
	return []State{StateStart, StateCompose, StateModel, StateGate, StateTools, StateVerify,
		StateSuspended, StateClosing, StateDone, StateStopped, StateFailed}
}
