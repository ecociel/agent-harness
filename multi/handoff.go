package multi

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/tool"
)

// Handoff lets peer agents transfer the conversation to each other. Unlike
// a sub-agent call, control moves and does not come back: the new owner sees
// the whole transcript and answers the user itself.
//
//	triage ──transfer_to_billing──▶ billing ──transfer_to_tech──▶ tech → answer
//
// Each agent gets one transfer_to_<name> tool per peer. A transfer ends the
// current agent's run with StopHandoff (the tool returns *tool.Handoff); the
// pattern then starts the target with the transcript plus a note. Hops are
// counted outside the agents, so a ping-pong between two agents is stopped
// with StopLoop after MaxHops.
type Handoff struct {
	Name   string
	Agents []*harness.Agent
	// Start is the first owner (default Agents[0]).
	Start string
	// MaxHops caps transfers per Run (default 4).
	MaxHops int
	// Sticky keeps the last owner for the next Run (chat sessions).
	Sticky bool

	once   sync.Once
	byName map[string]*harness.Agent
	err    error
	mu     sync.Mutex
	owner  string
}

// RunnerName implements harness.Named.
func (h *Handoff) RunnerName() string { return h.Name }

// Owner returns the agent that currently owns the conversation.
func (h *Handoff) Owner() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.owner
}

func (h *Handoff) build() {
	h.byName = map[string]*harness.Agent{}
	for _, a := range h.Agents {
		var extra []tool.Tool
		for _, peer := range h.Agents {
			if peer.Name != a.Name {
				extra = append(extra, transferTool(peer))
			}
		}
		reg, err := a.Tools.With(extra...)
		if err != nil {
			h.err = err
			return
		}
		cp := *a
		cp.Tools = reg
		h.byName[a.Name] = &cp
	}
}

func transferTool(peer *harness.Agent) tool.Tool {
	spec := tool.Spec{
		Name:        "transfer_to_" + peer.Name,
		Description: fmt.Sprintf("Hand the conversation to %s. %s", peer.Name, firstLine(peer.Instructions)),
		Parameters:  json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"}}}`),
	}
	return tool.Func(spec, func(_ context.Context, raw json.RawMessage) (string, error) {
		var a struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(raw, &a)
		return "", &tool.Handoff{Target: peer.Name, Note: a.Reason}
	})
}

// Run implements harness.Runner.
func (h *Handoff) Run(ctx context.Context, in harness.Input) (*harness.Result, error) {
	h.once.Do(h.build)
	s := open(ctx, "handoff", h.Name)
	if h.err != nil {
		return s.fail(in, h.err)
	}
	h.mu.Lock()
	owner := h.owner
	if owner == "" || !h.Sticky {
		owner = h.Start
		if owner == "" && len(h.Agents) > 0 {
			owner = h.Agents[0].Name
		}
	}
	h.mu.Unlock()
	msgs := in.Messages
	maxHops := def(h.MaxHops, 4)
	for hops := 0; ; {
		agent, ok := h.byName[owner]
		if !ok {
			return s.fail(in, fmt.Errorf("handoff: unknown agent %q", owner))
		}
		s.emit("owner", owner, nil)
		res, err := s.run(agent, harness.Input{Messages: msgs})
		if err != nil {
			return s.fail(in, err)
		}
		if res.Stop != harness.StopHandoff || res.Handoff == nil {
			h.mu.Lock()
			h.owner = owner
			h.mu.Unlock()
			out, _ := s.done(in, res.Output, res.Stop, res.Detail, nil)
			out.Agent, out.Messages = owner, res.Messages
			return out, nil
		}
		hops++
		target := res.Handoff.Target
		s.emit("transfer", owner+" → "+target, map[string]any{"note": res.Handoff.Note, "hop": hops})
		if hops > maxHops {
			return s.done(in, "", harness.StopLoop, fmt.Sprintf("more than %d handoffs", maxHops), nil)
		}
		msgs = append(res.Messages, model.User(fmt.Sprintf("[%s handed this conversation to you (%s). Note: %s] Please continue.",
			owner, target, res.Handoff.Note)))
		owner = target
	}
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}
