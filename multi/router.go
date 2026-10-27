package multi

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

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
// drops a request (ecociel/agents routing; tessmir "safe fallback").
type Router struct {
	Name       string
	Classifier harness.Runner
	Routes     map[string]harness.Runner
	Fallback   harness.Runner
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
func (rt *Router) Run(ctx context.Context, in harness.Input) (*harness.Result, error) {
	s := open(ctx, "router", rt.Name)
	labels := rt.Labels()
	prompt := fmt.Sprintf("Classify the request into exactly one of: %s.\nAnswer with the label only.\n\nRequest:\n%s",
		strings.Join(labels, ", "), in.Text())
	label := ""
	if res, err := s.run(rt.Classifier, harness.Prompt(prompt)); err == nil && res.OK() {
		label = parseLabel(res.Output, labels)
	}
	target, ok := rt.Routes[label]
	if !ok {
		if rt.Fallback == nil {
			return s.done(in, "", harness.StopFailed, "unroutable request and no fallback", nil)
		}
		label, target = "fallback", rt.Fallback
	}
	s.emit("route", label, map[string]any{"target": harness.NameOf(target)})
	res, err := s.run(target, in)
	if err != nil {
		return s.fail(in, err)
	}
	if !res.OK() {
		return s.stopped(in, label, res)
	}
	return s.ok(in, res.Output)
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
