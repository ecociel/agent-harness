// Package tool defines what the harness can call on the model's behalf: a
// Tool (spec plus behaviour), a Registry that is the single dispatch point,
// and Handoff, the control signal a tool uses to transfer the conversation to
// another agent.
//
// Tools carry a Risk and an optional Scope. The harness core does not
// interpret either; policy hooks (hooks/approval, hooks/guard) do. Keeping the
// metadata next to the behaviour means a policy can never see a tool the
// registry does not know.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/ecociel/agent-harness/model"
)

// Risk classifies what a tool can do to the outside world.
type Risk int

// Risk levels, ordered: a policy can say "anything at or above Write needs
// approval".
const (
	RiskRead      Risk = iota // reads only; safe to call freely
	RiskWrite                 // changes state that can be undone
	RiskDangerous             // irreversible or externally visible
)

// String returns "read", "write" or "dangerous".
func (r Risk) String() string {
	switch r {
	case RiskRead:
		return "read"
	case RiskWrite:
		return "write"
	case RiskDangerous:
		return "dangerous"
	}
	return fmt.Sprintf("risk(%d)", int(r))
}

// Spec describes a tool to the model and to policy hooks.
type Spec struct {
	Name        string
	Description string
	// Parameters is a JSON Schema object. Nil means "no arguments".
	Parameters json.RawMessage
	Risk       Risk
	// Scope, if set, is the permission a caller must hold (see
	// hooks/approval). Empty means no scope check.
	Scope string
}

// ModelSpec is the subset of s that is sent to the model.
func (s Spec) ModelSpec() model.ToolSpec {
	params := s.Parameters
	if params == nil {
		params = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return model.ToolSpec{Name: s.Name, Description: s.Description, Parameters: params}
}

// Tool is something the model can call.
//
// Call receives the raw JSON arguments and returns the text that goes back to
// the model. An ordinary error is reported to the model as the tool result
// (so it can recover); a *Handoff error ends the run and transfers control.
type Tool interface {
	Spec() Spec
	Call(ctx context.Context, args json.RawMessage) (string, error)
}

// Func builds a Tool from a spec and a function.
func Func(spec Spec, fn func(ctx context.Context, args json.RawMessage) (string, error)) Tool {
	return funcTool{spec: spec, fn: fn}
}

type funcTool struct {
	spec Spec
	fn   func(ctx context.Context, args json.RawMessage) (string, error)
}

func (t funcTool) Spec() Spec { return t.spec }
func (t funcTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	return t.fn(ctx, args)
}

// Registry is an ordered set of tools keyed by name. The zero value is ready
// to use. A Registry is not safe for concurrent mutation; build it before the
// agent runs.
type Registry struct {
	byName map[string]Tool
	order  []string
}

// NewRegistry returns a registry holding tools. It panics on a duplicate
// name, which is a programming error.
func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{}
	for _, t := range tools {
		r.MustAdd(t)
	}
	return r
}

// Add registers t. It fails if the name is empty or already taken.
func (r *Registry) Add(t Tool) error {
	name := t.Spec().Name
	if name == "" {
		return errors.New("tool: empty name")
	}
	if r.byName == nil {
		r.byName = map[string]Tool{}
	}
	if _, dup := r.byName[name]; dup {
		return fmt.Errorf("tool: duplicate %q", name)
	}
	r.byName[name] = t
	r.order = append(r.order, name)
	return nil
}

// MustAdd is Add that panics on error.
func (r *Registry) MustAdd(t Tool) {
	if err := r.Add(t); err != nil {
		panic(err)
	}
}

// Get returns the tool called name.
func (r *Registry) Get(name string) (Tool, bool) {
	if r == nil {
		return nil, false
	}
	t, ok := r.byName[name]
	return t, ok
}

// Specs returns all specs in registration order.
func (r *Registry) Specs() []Spec {
	if r == nil {
		return nil
	}
	out := make([]Spec, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n].Spec())
	}
	return out
}

// All returns the tools in registration order.
func (r *Registry) All() []Tool {
	if r == nil {
		return nil
	}
	out := make([]Tool, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n])
	}
	return out
}

// With returns a new registry holding r's tools followed by extra.
func (r *Registry) With(extra ...Tool) (*Registry, error) {
	out := &Registry{}
	for _, t := range append(r.All(), extra...) {
		if err := out.Add(t); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Names returns the registered names, sorted.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	out := append([]string(nil), r.order...)
	sort.Strings(out)
	return out
}

// Len is the number of tools.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.order)
}

// ErrUnknown is returned (wrapped) when the model calls a tool that is not
// registered.
var ErrUnknown = errors.New("tool: unknown tool")

// Call dispatches one call by name.
func (r *Registry) Call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	t, ok := r.Get(name)
	if !ok {
		return "", fmt.Errorf("%w %q", ErrUnknown, name)
	}
	return t.Call(ctx, args)
}

// Handoff is returned by a tool to transfer control of the conversation to
// another agent (see multi.Handoff). The run that receives it stops with
// StopHandoff and records Target and Note in its Result.
type Handoff struct {
	Target string
	Note   string
}

func (h *Handoff) Error() string { return "handoff to " + h.Target }

// AsHandoff reports whether err is (or wraps) a *Handoff.
func AsHandoff(err error) (*Handoff, bool) {
	var h *Handoff
	ok := errors.As(err, &h)
	return h, ok
}
