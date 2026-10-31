package multi

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ecociel/agent-harness"
)

// Router classifies the input and dispatches it to exactly one route.
//
//	in → classifier → label ─┬→ route A
//	                         ├→ route B
//	                         └→ fallback (unknown label or classifier failure)
//
// The classifier is a cheap Runner asked to answer with one label from the
// closed set of route names. Anything unroutable — an unknown label, a
// stopped or failed classifier — goes to Fallback, so the router never
// drops a request (ecociel/agents routing; tessmir "safe fallback"). A classifier
// that waits suspends the router: it has not failed.
type Router struct {
	Name       string
	Classifier harness.Runner
	Routes     map[string]harness.Runner
	Fallback   harness.Runner
	// Journal makes the pattern durable. Nil inherits, like Agent.Journal.
	Journal harness.Journal
	// Hooks attach to the pattern's own run. A Redactor here covers every part.
	Hooks []harness.Hook
	// Lease is the same as Parallel.Lease.
	Lease time.Duration
}

// RunnerName implements harness.Named.
func (rt *Router) RunnerName() string { return rt.Name }

// Labels returns the route names, sorted.
func (rt *Router) Labels() []string {
	ls := make([]string, 0, len(rt.Routes))
	for k := range rt.Routes {
		ls = append(ls, k)
	}
	sort.Strings(ls)
	return ls
}

// Run implements harness.Runner.
func (rt *Router) Run(ctx context.Context, in harness.Input) (*harness.Run, error) {
	parts := []part{{"classify", rt.Classifier}}
	for _, l := range rt.Labels() {
		parts = append(parts, part{"route/" + l, rt.Routes[l]})
	}
	if rt.Fallback != nil {
		parts = append(parts, part{"fallback", rt.Fallback})
	}
	spec := pattern{kind: "router", name: rt.Name, journal: rt.Journal, hooks: rt.Hooks, lease: rt.Lease, parts: parts}
	return spec.run(ctx, in, rt.body)
}

func (rt *Router) body(s *scope) (string, harness.Spend, error) {
	labels := rt.Labels()
	prompt := fmt.Sprintf("Classify the request into exactly one of: %s.\nAnswer with the label only.\n\nRequest:\n%s",
		strings.Join(labels, ", "), s.in.Text())
	res, err := s.advance(rt.Classifier, s.at("classify", harness.Prompt(prompt)))
	switch {
	case res == nil:
		return s.halt(err)
	case res.State == harness.StateSuspended:
		return s.park(res.Waits...)
	}
	label := ""
	if err == nil && res.OK() {
		label = parseLabel(res.Output, labels)
	}
	target, ok := rt.Routes[label]
	id := "route/" + label
	if !ok {
		if rt.Fallback == nil {
			return s.fail(errors.New("unroutable request and no fallback"))
		}
		label, target, id = "fallback", rt.Fallback, "fallback"
	}
	s.emit("route", label)
	out, err := s.need(label, target, s.at(id, s.in))
	if err != nil {
		return s.exit(err)
	}
	return s.ok(out.Output)
}

func parseLabel(out string, labels []string) string {
	words := strings.FieldsFunc(strings.ToLower(out), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-'
	})
	for _, w := range words {
		if slices.Contains(labels, w) {
			return w
		}
	}
	return ""
}
