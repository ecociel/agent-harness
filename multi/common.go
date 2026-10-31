package multi

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ecociel/agent-harness"
)

// part is one child of a pattern. Its run ID is the pattern's plus "/" and id, taken
// from its position, so an ID never depends on which goroutine ran first.
type part struct {
	id     string
	runner harness.Runner
}

// pattern is what every multi-agent pattern has in common: it is a composite run
// whose body walks its children. The body keeps no state. It runs again after a crash
// or a signal, and the children's IDs come from their position: finished ones return
// from their journals and the rest carry on.
type pattern struct {
	kind, name string
	journal    harness.Journal
	hooks      []harness.Hook
	lease      time.Duration
	parts      []part // every child a run can have, which a cancel walks
	roles      []part // what the shape pins, if not parts: a grid's rows repeat the same runners
}

// run advances the pattern.
func (p pattern) run(ctx context.Context, in harness.Input, body func(*scope) (string, harness.Spend, error)) (*harness.Run, error) {
	if p.name == "" {
		p.name = p.kind
	}
	c := &harness.Composite{Name: p.name, Shape: p.shape(), Hooks: p.hooks, Journal: p.journal, Lease: p.lease,
		Body: func(ctx context.Context, in harness.Input) (string, harness.Spend, error) {
			return body(&scope{id: in.RunID, name: p.name, kind: p.kind, ctx: ctx,
				in: harness.Input{Messages: in.Messages}, signals: in.Signals})
		},
		Cancel: func(ctx context.Context, id, reason string) error {
			for _, c := range p.parts {
				if err := cancelChild(ctx, c.runner, id+"/"+c.id, reason); err != nil {
					return err
				}
			}
			return nil
		}}
	return c.Run(ctx, in)
}

// shape names the parts by position, so that moving an agent changes it.
func (p pattern) shape() string {
	roles := p.roles
	if roles == nil {
		roles = p.parts
	}
	names := make([]string, len(roles))
	for i, c := range roles {
		names[i] = c.id + ":" + harness.NameOf(c.runner)
	}
	return p.kind + ": " + strings.Join(names, ", ")
}

// scope is one execution of a pattern's body.
type scope struct {
	id, name, kind string
	ctx            context.Context // children run with this context
	in             harness.Input   // the pattern's input, from the journal
	signals        []harness.Signal

	mu      sync.Mutex // children may finish concurrently
	below   harness.Spend
	partial string // what a stopped child had written
}

// fork names a child by its position in the pattern.
func (s *scope) fork(part string) string { return s.id + "/" + part }

// at gives an input the run ID of the child at part.
func (s *scope) at(part string, in harness.Input) harness.Input {
	in.RunID = s.fork(part)
	return in
}

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
	s.below = s.below.Add(res.Spent())
	return res, err
}

// need advances a child the pattern cannot go on without. A nil Run comes with the
// error that must end the body: pass it to exit.
func (s *scope) need(who string, r harness.Runner, in harness.Input) (*harness.Run, error) {
	res, err := s.advance(r, in)
	switch {
	case res == nil:
		return nil, err
	case res.State == harness.StateSuspended:
		return nil, harness.Suspend(res.Waits...)
	case err != nil:
		return nil, harness.Stop(harness.StopFailed, fmt.Sprintf("%s: %v", who, err))
	case !res.OK():
		s.partial = res.Output
		return nil, harness.Stop(res.Stop, fmt.Sprintf("%s stopped: %s %s", who, res.Stop, res.Detail))
	}
	return res, nil
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

// cancelChild ends one child. It may never have started. Or a live worker may have
// moved it on in the same instant, and then that worker meets the cancel at its next
// step, so losing the race is not a failure.
func cancelChild(ctx context.Context, r harness.Runner, id, reason string) error {
	res, err := r.Run(ctx, harness.Input{RunID: id, Cancel: reason})
	if res != nil || errors.Is(err, harness.ErrNoSuchRun) || errors.Is(err, harness.ErrFenced) {
		return nil
	}
	return err
}

func stopped(err error) error {
	if errors.Is(err, harness.ErrFenced) || errors.Is(err, harness.ErrDrained) {
		return err
	}
	return fmt.Errorf("%w: %w", harness.ErrDrained, err)
}

// ok ends the body with an answer.
func (s *scope) ok(output string) (string, harness.Spend, error) { return output, s.below, nil }

// exit ends the body with what need returned.
func (s *scope) exit(err error) (string, harness.Spend, error) { return s.partial, s.below, err }

// halt stops the worker, not the pattern.
func (s *scope) halt(err error) (string, harness.Spend, error) { return "", s.below, err }

func (s *scope) fail(err error) (string, harness.Spend, error) {
	return "", s.below, harness.Stop(harness.StopFailed, err.Error())
}

// stoppedChild stops the pattern for the reason its child stopped.
func (s *scope) stoppedChild(who string, res *harness.Run) (string, harness.Spend, error) {
	return res.Output, s.below, harness.Stop(res.Stop, fmt.Sprintf("%s stopped: %s %s", who, res.Stop, res.Detail))
}

// park suspends the pattern on its children's waits.
func (s *scope) park(waits ...harness.Wait) (string, harness.Spend, error) {
	return "", s.below, harness.Suspend(waits...)
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

func def(v, d int) int {
	if v <= 0 {
		return d
	}
	return v
}

// numbers returns prefix0, prefix1, and so on.
func numbers(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = prefix + strconv.Itoa(i)
	}
	return out
}

// positional gives runners the IDs of their positions.
func positional(rs []harness.Runner) []part {
	out := make([]part, len(rs))
	for i, r := range rs {
		out[i] = part{strconv.Itoa(i), r}
	}
	return out
}
