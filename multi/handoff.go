package multi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

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
//
// A child's run ID names its hop and its agent, and a transfer is read from the
// recorded run, so a resume walks the same hops. The owner's reply carries its name as
// provenance, which is how Sticky finds the owner in a session's history, on any
// process. A transfer drops provider state: the new owner has other instructions and
// tools, so state like an Anthropic signature could not be valid for it.
type Handoff struct {
	Name   string
	Agents []*harness.Agent
	// Start is the first owner (default Agents[0]).
	Start string
	// MaxHops caps transfers per Run (default 4).
	MaxHops int
	// Sticky keeps the last owner for the next Run (chat sessions).
	Sticky bool
	// Journal makes the pattern durable. Nil inherits, like Agent.Journal.
	Journal harness.Journal
	// Hooks attach to the pattern's own run. A Redactor here covers every part.
	Hooks []harness.Hook
	// Lease is the same as Parallel.Lease.
	Lease time.Duration

	once   sync.Once
	byName map[string]*harness.Agent
	err    error
}

// RunnerName implements harness.Named.
func (h *Handoff) RunnerName() string { return h.Name }

const ownerSource = "handoff:"

// Owner returns the agent that last answered in msgs, or "".
func (h *Handoff) Owner(msgs []model.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if name, ok := strings.CutPrefix(msgs[i].Source, ownerSource); ok {
			return name
		}
	}
	return ""
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

// A transfer has no effect of its own, so repeating one after a crash is harmless.
func transferTool(peer *harness.Agent) tool.Tool {
	spec := tool.Spec{
		Name:        "transfer_to_" + peer.Name,
		Description: fmt.Sprintf("Hand the conversation to %s. %s", peer.Name, firstLine(peer.Instructions)),
		Parameters:  json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"}}}`),
		Effect:      tool.EffectIdempotent,
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
func (h *Handoff) Run(ctx context.Context, in harness.Input) (*harness.Run, error) {
	h.once.Do(h.build)
	if h.err != nil {
		return nil, h.err
	}
	maxHops := def(h.MaxHops, 4)
	var roles, parts []part
	for _, a := range h.Agents {
		roles = append(roles, part{a.Name, h.byName[a.Name]})
		for hop := range maxHops + 1 {
			parts = append(parts, part{hopID(hop, a.Name), h.byName[a.Name]})
		}
	}
	spec := pattern{kind: "handoff", name: h.Name, journal: h.Journal, hooks: h.Hooks, lease: h.Lease, parts: parts, roles: roles}
	return spec.run(ctx, in, func(s *scope) (string, harness.Spend, error) { return h.body(s, maxHops) })
}

func hopID(hop int, agent string) string { return fmt.Sprintf("hop/%d/%s", hop, agent) }

func (h *Handoff) first(msgs []model.Message) string {
	if h.Sticky {
		if owner := h.Owner(msgs); h.byName[owner] != nil {
			return owner
		}
	}
	if h.Start != "" || len(h.Agents) == 0 {
		return h.Start
	}
	return h.Agents[0].Name
}

func (h *Handoff) body(s *scope, maxHops int) (string, harness.Spend, error) {
	owner, msgs := h.first(s.in.Messages), s.in.Messages
	for hop := 0; ; hop++ {
		agent, ok := h.byName[owner]
		if !ok {
			return s.fail(fmt.Errorf("handoff: unknown agent %q", owner))
		}
		s.emit("owner", owner)
		res, err := s.advance(agent, s.at(hopID(hop, owner), harness.Input{Messages: msgs}))
		switch {
		case res == nil:
			return s.halt(err)
		case res.State == harness.StateSuspended:
			return s.park(res.Waits...)
		case res.Stop == harness.StopHandoff && res.Handoff != nil:
		case err != nil:
			return s.fail(fmt.Errorf("%s: %w", owner, err))
		case res.OK():
			harness.Attribute(s.ctx, ownerSource+owner)
			return s.ok(res.Output)
		default:
			return s.stoppedChild(owner, res)
		}
		if hop >= maxHops {
			return "", s.below, harness.Stop(harness.StopLoop, fmt.Sprintf("more than %d handoffs", maxHops))
		}
		target := res.Handoff.Target
		s.emit("transfer", owner+" → "+target)
		msgs = append(transcript(res.Messages), model.User(fmt.Sprintf("[%s handed this conversation to you (%s). Note: %s] Please continue.",
			owner, target, res.Handoff.Note)))
		owner = target
	}
}

// transcript is a run's history without the state only its own owner could use.
func transcript(msgs []model.Message) []model.Message {
	out := append([]model.Message(nil), msgs...)
	for i := range out {
		out[i].Opaque = nil
	}
	return out
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
