package eval

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/ecociel/agent-harness"
)

// Surfaces is the Facts key under which a consumer lists, as a []string, the texts
// its agent acted on outside tool arguments: memory it wrote, notes it scheduled.
// not_echoed reads them together with every call's arguments.
const Surfaces = "surfaces"

// Observation is what checks read: one Exchange per scenario turn, in turn order,
// plus the facts the consumer observed outside the runs.
type Observation struct {
	Turns []Exchange     `json:"turns"`
	Facts map[string]any `json:"facts,omitempty"`
}

// Exchange is one turn's run as checks see it.
type Exchange struct {
	// Answer is the run's output, or the last text the model wrote when the run
	// ended without one, as a suspended run does.
	Answer string             `json:"answer"`
	Calls  []Call             `json:"calls"`
	Waits  []Wait             `json:"waits"`
	Stop   harness.StopReason `json:"stop"`
	Spend  harness.Spend      `json:"spend"`
	// Latency is how long the turn took to come back, waits answered in between
	// included.
	Latency time.Duration `json:"latency_ns"`
	// Run is the folded run. trajectory.From(*Run) gives its messages and steps.
	Run *harness.Run `json:"-"`
}

// Call is one tool call the model asked for, whether or not it ran.
type Call struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
	// Outcome is the last recorded one: "ok", "error", "parked", "denied", "lost",
	// or empty for a call that never got that far.
	Outcome string `json:"outcome,omitempty"`
	Result  string `json:"result,omitempty"`
}

// Wait is a wait the run opened and how it was answered.
type Wait struct {
	ID     string `json:"id"`
	Tool   string `json:"tool,omitempty"` // empty for a wait on no call, such as a retry timer
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
	// Answer is the signal value. Empty while the wait is open or after it expired.
	Answer  string `json:"answer,omitempty"`
	Expired bool   `json:"expired,omitempty"`
}

// Open reports whether nobody answered w and it did not expire.
func (w Wait) Open() bool { return w.Answer == "" && !w.Expired }

// Observe reads one turn's run. r must carry its Events, as a Run from Agent.Run
// or Fold does.
func Observe(r *harness.Run) Exchange {
	x := Exchange{Run: r, Stop: r.Stop, Spend: r.Spent(), Answer: r.Output}
	type key struct{ turn, index int }
	calls := map[key]int{}
	waits := map[string]int{}
	tool := func(turn, index int) string {
		if i, ok := calls[key{turn, index}]; ok {
			return x.Calls[i].Tool
		}
		return ""
	}
	lastText := ""
	for _, e := range r.Events {
		at, known := calls[key{e.Turn, e.Index}]
		switch e.Kind {
		case harness.KindModelDone:
			if e.Reply == nil {
				continue
			}
			if strings.TrimSpace(e.Reply.Text) != "" {
				lastText = e.Reply.Text
			}
			for i, c := range e.Reply.ToolCalls {
				calls[key{e.Turn, i}] = len(x.Calls)
				x.Calls = append(x.Calls, Call{Tool: c.Name, Args: c.Arguments})
			}
		case harness.KindToolGated:
			if known && e.Decision != nil && e.Decision.Action == harness.ActionDeny {
				x.Calls[at].Outcome, x.Calls[at].Result = "denied", e.Text
			}
		case harness.KindToolDone:
			if known {
				x.Calls[at].Outcome, x.Calls[at].Result = e.Outcome, e.Text
			}
		case harness.KindToolLost:
			if known {
				x.Calls[at].Outcome = "lost"
			}
		case harness.KindWaitResolved:
			if i, ok := waits[e.Signal.Wait]; ok {
				x.Waits[i].Answer, x.Waits[i].Expired = e.Signal.Value, e.Signal.Expired
			}
		}
		for _, w := range e.Waits {
			name := ""
			if w.Call >= 0 {
				name = tool(e.Turn, w.Call)
			}
			waits[w.ID] = len(x.Waits)
			x.Waits = append(x.Waits, Wait{ID: w.ID, Tool: name, Kind: w.Kind, Detail: w.Detail})
		}
	}
	for _, w := range r.Waits {
		if _, seen := waits[w.ID]; seen || w.Run != r.ID {
			continue
		}
		name := ""
		if w.Call >= 0 {
			name = tool(r.Turn, w.Call)
		}
		x.Waits = append(x.Waits, Wait{ID: w.ID, Tool: name, Kind: w.Kind, Detail: w.Detail})
	}
	if x.Answer == "" {
		x.Answer = lastText
	}
	return x
}

// Answer is the last turn's answer.
func (o Observation) Answer() string {
	if len(o.Turns) == 0 {
		return ""
	}
	return o.Turns[len(o.Turns)-1].Answer
}

// Answers are every turn's answer, in turn order.
func (o Observation) Answers() []string {
	out := make([]string, len(o.Turns))
	for i, t := range o.Turns {
		out[i] = t.Answer
	}
	return out
}

// Calls are every turn's calls, in order.
func (o Observation) Calls() []Call {
	var out []Call
	for _, t := range o.Turns {
		out = append(out, t.Calls...)
	}
	return out
}

// Waits are every turn's waits, in order.
func (o Observation) Waits() []Wait {
	var out []Wait
	for _, t := range o.Turns {
		out = append(out, t.Waits...)
	}
	return out
}

// Spend is what every turn spent.
func (o Observation) Spend() harness.Spend {
	var s harness.Spend
	for _, t := range o.Turns {
		s = s.Add(t.Spend)
	}
	return s
}

// Latency is the sum of every turn's latency.
func (o Observation) Latency() time.Duration {
	var d time.Duration
	for _, t := range o.Turns {
		d += t.Latency
	}
	return d
}
