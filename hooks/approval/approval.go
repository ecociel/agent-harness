// Package approval is the security, policy and approval hook — our reading
// of "SPA" in the original brief (see README, open questions).
//
// It decides, before a tool runs, whether this caller may run this tool now:
//
//  1. Scope: a tool whose Spec.Scope is set requires the caller's Principal
//     (from the context) to hold that scope. Authority is checked below the
//     model, so a prompt-injected model cannot talk its way past it
//     (confused-deputy defence, as in ecociel/agents cmd/patterns/identity).
//  2. Approval: a tool at or above Policy.Threshold risk needs a yes. An Approver
//     decides on the spot (a rule, a second model). With Ask set the run suspends
//     for a person instead, and nothing blocks while it waits. Neither means no.
//  3. Audit: every decision, allowed or not, is passed to Policy.Audit. Decisions
//     are re-made after a crash, a claim or a resolved wait, so a record can repeat.
//     The journal's tool.gated events are the durable record; Audit is a feed.
//
// Denials are fed back to the model (it can explain or choose another way);
// set HaltOnDeny to stop the run instead.
package approval

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/tool"
)

// Principal is who the run acts for.
type Principal struct {
	ID     string
	Scopes []string
}

// Has reports whether p holds scope.
func (p Principal) Has(scope string) bool { return slices.Contains(p.Scopes, scope) }

type ctxKey struct{}

// WithPrincipal attaches p to ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom returns the principal on ctx and whether there was one.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Request is what an Approver is asked to decide.
type Request struct {
	RunID     string
	Agent     string
	Principal Principal
	Tool      tool.Spec
	Call      model.ToolCall
}

// Approver decides a Request. reason is shown to the model on denial.
type Approver interface {
	Approve(ctx context.Context, req Request) (ok bool, reason string)
}

// ApproverFunc adapts a function to Approver.
type ApproverFunc func(ctx context.Context, req Request) (bool, string)

// Approve calls f.
func (f ApproverFunc) Approve(ctx context.Context, req Request) (bool, string) { return f(ctx, req) }

// Record is one audited decision.
type Record struct {
	Time      time.Time `json:"time"`
	RunID     string    `json:"run_id"`
	Principal string    `json:"principal"`
	Tool      string    `json:"tool"`
	Risk      string    `json:"risk"`
	Decision  string    `json:"decision"` // allow | deny
	Rule      string    `json:"rule"`     // scope | approval | approved | default
	Reason    string    `json:"reason,omitempty"`
}

// WaitKind is the wait kind Ask opens. Approved is the signal value that means yes.
const (
	WaitKind = "approval"
	Approved = "yes"
)

// Policy is the hook.
type Policy struct {
	// Threshold is the lowest risk that needs approval. The zero value,
	// RiskRead, means every tool needs approval (a safe default); use New
	// for the usual RiskWrite, and tool.RiskDangerous+1 to disable approvals.
	Threshold tool.Risk
	// Approver decides calls at or above Threshold. Nil denies them, unless Ask is
	// set.
	Approver Approver
	// Ask suspends the run on a WaitKind wait instead of calling Approver. Any
	// process resumes it with a Signal whose Value is Approved or anything else.
	Ask bool
	// TTL expires an unanswered ask. Zero never does. An expired ask is asked again.
	TTL time.Duration
	// Audit, if set, receives every decision.
	Audit func(Record)
	// HaltOnDeny stops the run on a denial instead of informing the model.
	HaltOnDeny bool
}

// New returns a policy that needs approval at or above threshold.
func New(threshold tool.Risk, approver Approver) *Policy {
	return &Policy{Threshold: threshold, Approver: approver}
}

// Name implements harness.Hook.
func (p *Policy) Name() string { return "approval" }

// BeforeTool applies scope, approval and audit.
func (p *Policy) BeforeTool(ctx context.Context, r harness.Run, c harness.Call) harness.Decision {
	spec := c.Spec
	if spec.Name == "" {
		return harness.Allow() // the registry reports unknown tools itself
	}
	pr, _ := PrincipalFrom(ctx)
	rec := Record{Time: time.Now(), RunID: r.ID, Principal: pr.ID, Tool: spec.Name, Risk: spec.Risk.String()}
	decide := func(allow bool, rule, reason string) harness.Decision {
		rec.Rule, rec.Reason = rule, reason
		rec.Decision = "allow"
		if !allow {
			rec.Decision = "deny"
		}
		if p.Audit != nil {
			p.Audit(rec)
		}
		note := rec.Decision + " " + spec.Name + " (" + spec.Risk.String() + ", " + rule
		if reason != "" {
			note += ": " + reason
		}
		harness.Note(ctx, "%s)", note)
		switch {
		case allow:
			return harness.Allow()
		case p.HaltOnDeny:
			return harness.Decision{Action: harness.ActionHalt, Reason: reason, Stop: harness.StopPolicy}
		default:
			return harness.Deny(reason)
		}
	}
	if spec.Scope != "" && !pr.Has(spec.Scope) {
		return decide(false, "scope", fmt.Sprintf("caller %q lacks scope %q", pr.ID, spec.Scope))
	}
	if spec.Risk < p.Threshold {
		return decide(true, "default", "")
	}
	if p.Ask {
		s, ok := c.Signal(WaitKind)
		switch {
		case !ok || s.Expired:
			return harness.Await(harness.Wait{Kind: WaitKind, Detail: fmt.Sprintf("%s tool %q needs approval", spec.Risk, spec.Name), TTL: p.TTL})
		case s.Value != Approved:
			return decide(false, "approval", "declined by "+s.By)
		}
		return decide(true, "approved", "by "+s.By)
	}
	if p.Approver == nil {
		return decide(false, "approval", fmt.Sprintf("%s tool %q needs approval and no approver is configured", spec.Risk, spec.Name))
	}
	okd, reason := p.Approver.Approve(ctx, Request{RunID: r.ID, Agent: r.Agent, Principal: pr, Tool: spec, Call: c.ToolCall})
	if !okd {
		if reason == "" {
			reason = "not approved"
		}
		return decide(false, "approval", reason)
	}
	return decide(true, "approved", reason)
}
