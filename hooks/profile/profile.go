// Package profile applies per-model profiles — the "model profiles" idea from
// the harness talk: keep the harness general, but let each model family see
// the tools and instructions it was trained on.
//
// The canonical example is file editing: one model family is best with a
// string-replace editor, another with a patch tool. Register both tools and
// let the profile hide the one the current model is out of distribution on.
// A hidden tool is also refused if the model calls it anyway.
package profile

import (
	"context"
	"slices"
	"strings"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
)

// Profile adjusts requests for models whose name starts with Match.
type Profile struct {
	Match string
	// Instructions are appended to the system message.
	Instructions string
	// Hide removes these tools from the request.
	Hide []string
	// MaxTokens overrides the output cap when non-zero.
	MaxTokens int
}

// Profiles is the hook: the first matching profile applies.
type Profiles []Profile

// Name implements harness.Hook.
func (ps Profiles) Name() string { return "profile" }

func (ps Profiles) find(modelName string) (Profile, bool) {
	for _, p := range ps {
		if strings.HasPrefix(modelName, p.Match) {
			return p, true
		}
	}
	return Profile{}, false
}

// BeforeModel applies the matching profile to req.
func (ps Profiles) BeforeModel(_ context.Context, r *harness.Run, req *model.Request) error {
	p, ok := ps.find(req.Model)
	if !ok {
		return nil
	}
	r.Set("profile", p.Match)
	r.Note(ps.Name(), "profile %q for model %q", p.Match, req.Model)
	if p.Instructions != "" {
		if len(req.Messages) > 0 && req.Messages[0].Role == model.RoleSystem {
			req.Messages[0].Content += "\n\n" + p.Instructions
		} else {
			req.Messages = append([]model.Message{model.System(p.Instructions)}, req.Messages...)
		}
	}
	if len(p.Hide) > 0 {
		req.Tools = slices.DeleteFunc(req.Tools, func(t model.ToolSpec) bool { return slices.Contains(p.Hide, t.Name) })
	}
	if p.MaxTokens > 0 {
		req.MaxTokens = p.MaxTokens
	}
	return nil
}

// BeforeTool refuses tools the profile hides.
func (ps Profiles) BeforeTool(_ context.Context, r *harness.Run, call model.ToolCall) harness.Decision {
	if p, ok := ps.find(r.Agent.Model); ok && slices.Contains(p.Hide, call.Name) {
		return harness.Deny("tool " + call.Name + " is not available for this model")
	}
	return harness.Allow()
}
