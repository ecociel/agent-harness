package harness

// State is one node of the run state machine. See the package documentation
// for the diagram; the legal edges are the transition table below and nothing
// else. The machine refuses any edge that is not in the table, so "that
// transition can never happen" is enforced at runtime instead of being a
// comment.
type State string

// The states. Start, Prepare and Verify (plus the terminals) form the outer
// loop: one task or episode. Compose, Model, Route, Tools and Observe form the
// inner loop: one model/tool turn, repeated until the model answers without
// asking for tools.
const (
	// StateStart is the initial state of every run.
	StateStart State = "start"
	// StatePrepare runs BeforeRun hooks: input guardrails, run spans, budget
	// admission. Outer loop.
	StatePrepare State = "prepare"
	// StateCompose builds the model request and runs BeforeModel hooks:
	// compaction/summarisation, model profiles, budget pre-checks. Inner loop.
	StateCompose State = "compose"
	// StateModel calls the provider through the WrapModel middleware chain,
	// streaming deltas to OnDelta hooks, then runs AfterModel hooks. Inner loop.
	StateModel State = "model"
	// StateRoute decides: tool calls go to StateTools, a plain answer goes to
	// StateVerify. No hooks; it is pure control flow. Inner loop.
	StateRoute State = "route"
	// StateTools executes the requested calls: BeforeTool (veto/approval),
	// the WrapTool chain, then AfterTool. Inner loop.
	StateTools State = "tools"
	// StateObserve is the turn boundary: AfterTurn hooks (loop detection,
	// checkpoints, metrics) before composing the next turn. Inner loop.
	StateObserve State = "observe"
	// StateVerify runs CheckOutput hooks on the candidate answer: output
	// guardrails and verifiers. A Revise verdict feeds back into
	// StateCompose; this edge is the outer loop's repair cycle.
	StateVerify State = "verify"
	// StateDone is the terminal state of a run that produced an accepted
	// answer.
	StateDone State = "done"
	// StateStopped is the terminal state of a run that a hook, a budget, a
	// limit, a handoff or cancellation ended on purpose.
	StateStopped State = "stopped"
	// StateFailed is the terminal state of a run that hit an infrastructure
	// error (provider down, hook bug).
	StateFailed State = "failed"
)

// Loop names which loop a state belongs to: "outer", "inner" or "terminal".
func (s State) Loop() string {
	switch s {
	case StateCompose, StateModel, StateRoute, StateTools, StateObserve:
		return "inner"
	case StateDone, StateStopped, StateFailed:
		return "terminal"
	}
	return "outer"
}

// Terminal reports whether s ends the run.
func (s State) Terminal() bool { return s.Loop() == "terminal" }

// transitions is the specification of the machine. Every non-terminal state
// may also go to StateStopped and StateFailed; those edges are added in init
// so the table below shows only the interesting flow.
var transitions = map[State][]State{
	StateStart:   {StatePrepare},
	StatePrepare: {StateCompose},
	StateCompose: {StateModel},
	StateModel:   {StateRoute},
	StateRoute:   {StateTools, StateVerify},
	StateTools:   {StateObserve},
	StateObserve: {StateCompose},
	StateVerify:  {StateDone, StateCompose},
}

func init() {
	for s, next := range transitions {
		transitions[s] = append(next, StateStopped, StateFailed)
	}
}

// CanTransition reports whether from → to is a legal edge.
func CanTransition(from, to State) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Transitions returns a copy of the transition table, for documentation,
// diagrams and the web diagnostic view.
func Transitions() map[State][]State {
	out := make(map[State][]State, len(transitions))
	for k, v := range transitions {
		out[k] = append([]State(nil), v...)
	}
	return out
}

// States lists every state in diagram order.
func States() []State {
	return []State{StateStart, StatePrepare, StateCompose, StateModel, StateRoute,
		StateTools, StateObserve, StateVerify, StateDone, StateStopped, StateFailed}
}
