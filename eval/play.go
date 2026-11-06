package eval

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
)

// Target is the system under test. The consumer implements it over its real
// stack: its inbox, workers, journal, tools and fixtures.
type Target interface {
	Open(ctx context.Context, s Scenario) (Conversation, error)
}

// Conversation is one scenario's conversation.
type Conversation interface {
	// Send plays one turn and returns once its run is terminal, or suspended on a
	// wait the turn's Answer does not cover. Send answers the waits Answer covers
	// itself and returns the run as it stands at the end, Events included.
	Send(ctx context.Context, t Turn) (*harness.Run, error)
	// Facts are what the consumer observed outside the runs, read once after the
	// last turn: rows written, mail sent. Its own kinds read them, and not_echoed
	// reads Facts[Surfaces].
	Facts(ctx context.Context) (map[string]any, error)
	Close(ctx context.Context) error
}

// Play runs one scenario through t. A turn that fails to run is an error, and the
// observation holds the turns before it.
func Play(ctx context.Context, t Target, s Scenario) (o Observation, err error) {
	conv, err := t.Open(ctx, s)
	if err != nil {
		return o, fmt.Errorf("open: %w", err)
	}
	defer func() { err = errors.Join(err, conv.Close(context.WithoutCancel(ctx))) }()
	for i, turn := range s.Turns {
		start := time.Now()
		r, err := conv.Send(ctx, turn)
		if err == nil && r == nil {
			err = errors.New("no run")
		}
		if err != nil {
			return o, fmt.Errorf("turn %d: %w", i+1, err)
		}
		x := Observe(r)
		x.Latency = time.Since(start)
		o.Turns = append(o.Turns, x)
	}
	if o.Facts, err = conv.Facts(ctx); err != nil {
		return o, fmt.Errorf("facts: %w", err)
	}
	return o, nil
}

// RunScenarios plays every scenario through t, up to parallel at once (default 4),
// grades each with kinds and returns the report. A scenario that does not
// Validate or fails to play is an error in the report, not a failure.
func RunScenarios(ctx context.Context, t Target, ss []Scenario, kinds Kinds, parallel int) ScenarioReport {
	if parallel <= 0 {
		parallel = 4
	}
	rep := ScenarioReport{Started: time.Now()}
	out := make([]Result, len(ss))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, s := range ss {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := Validate(s, kinds); err != nil {
				out[i] = grade(s, Observation{}, err, kinds)
				return
			}
			o, err := Play(ctx, t, s)
			out[i] = grade(s, o, err, kinds)
		}()
	}
	wg.Wait()
	for _, r := range out {
		rep.add(r)
	}
	rep.finish()
	return rep
}

func grade(s Scenario, o Observation, err error, kinds Kinds) Result {
	r := Result{Scenario: s.Name, Category: s.Category, Channel: s.Channel, Answer: o.Answer(), Tools: []string{},
		Spend: o.Spend(), Latency: o.Latency()}
	for _, c := range o.Calls() {
		r.Tools = append(r.Tools, c.Tool)
	}
	if err != nil {
		r.Err = err.Error()
		return r
	}
	r.Failed = Grade(s, o, kinds)
	r.Pass = len(r.Failed) == 0
	return r
}

// Provider returns a fake model that replies with the script in order. A call's
// Args may name vars as {{name}}; a name vars lacks is an error. The i-th reply's
// call has the ID "call_<i>", counting from 1.
func (sc Script) Provider(vars map[string]string) (*fake.Provider, error) {
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	slices.Sort(names)
	pairs := make([]string, 0, 2*len(names))
	for _, name := range names {
		pairs = append(pairs, "{{"+name+"}}", vars[name])
	}
	fill := strings.NewReplacer(pairs...)
	out := make([]model.Response, len(sc.Replies))
	for i, r := range sc.Replies {
		out[i] = model.Response{Text: r.Say}
		if r.Call == "" {
			continue
		}
		args := fill.Replace(r.Args)
		if args == "" {
			args = "{}"
		}
		if strings.Contains(args, "{{") {
			return nil, fmt.Errorf("reply %d: %s names a variable that is not set", i+1, args)
		}
		out[i].ToolCalls = []model.ToolCall{{ID: fmt.Sprintf("call_%d", i+1), Name: r.Call, Arguments: []byte(args)}}
	}
	return fake.Script(out...), nil
}

// SelfTest proves that the checks of every scenario tell a good transcript from a
// bad one. open returns a Target whose model replies with the script given. Every
// Good script must play and pass every check, and each Bad script must play and
// fail exactly the kinds in its Fails. Across all scenarios, every kind they use
// must be the only failure of at least one Bad script, so no kind is untested. It
// returns every problem it found.
func SelfTest(ctx context.Context, open func(Script) Target, ss []Scenario, kinds Kinds) error {
	var errs []error
	used := map[string]bool{}
	alone := map[string]bool{}
	for _, s := range ss {
		if err := Validate(s, kinds); err != nil {
			errs = append(errs, err)
			continue
		}
		if len(s.Selftest.Good) == 0 || len(s.Selftest.Bad) == 0 {
			errs = append(errs, fmt.Errorf("%s: the selftest needs a good and a bad script", s.Name))
		}
		for _, c := range s.Checks {
			used[c.Kind] = true
		}
		for i, sc := range s.Selftest.Good {
			o, err := Play(ctx, open(sc), s)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: good script %d: %w", s.Name, i+1, err))
				continue
			}
			for _, f := range Grade(s, o, kinds) {
				errs = append(errs, fmt.Errorf("%s: good script %d fails %s", s.Name, i+1, f))
			}
		}
		for i, b := range s.Selftest.Bad {
			o, err := Play(ctx, open(b.Script), s)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: bad script %d: %w", s.Name, i+1, err))
				continue
			}
			var failed []string
			for _, f := range Grade(s, o, kinds) {
				if !slices.Contains(failed, f.Kind) {
					failed = append(failed, f.Kind)
				}
			}
			want := slices.Clone(b.Fails)
			slices.Sort(failed)
			slices.Sort(want)
			want = slices.Compact(want)
			if !slices.Equal(failed, want) {
				errs = append(errs, fmt.Errorf("%s: bad script %d fails %v, want %v", s.Name, i+1, failed, want))
				continue
			}
			if len(failed) == 1 {
				alone[failed[0]] = true
			}
		}
	}
	var lonely []string
	for kind := range used {
		if !alone[kind] {
			lonely = append(lonely, kind)
		}
	}
	slices.Sort(lonely)
	for _, kind := range lonely {
		errs = append(errs, fmt.Errorf("no bad script fails %s alone", kind))
	}
	return errors.Join(errs...)
}
