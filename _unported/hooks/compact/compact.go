// Package compact keeps the conversation inside a context budget by
// summarising old turns before the model call — the "summarisation runs
// before the model" middleware from the harness talk.
//
// When the estimated size of the history exceeds MaxTokens, the oldest part
// is replaced by a single summary message and the most recent KeepLast
// messages are kept verbatim. The cut is moved back to a user message so a
// tool call is never separated from its result. The compaction is written
// back to Run.Messages, so it persists for the rest of the run (and the
// session).
package compact

import (
	"context"
	"fmt"
	"strings"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
)

// Summarizer turns old messages into a short text.
type Summarizer func(ctx context.Context, old []model.Message) (string, error)

// Compactor is the hook.
type Compactor struct {
	// MaxTokens triggers compaction when the estimated history exceeds it.
	MaxTokens int
	// KeepLast messages are always kept verbatim (default 6).
	KeepLast int
	// Summarize builds the summary; nil uses Digest.
	Summarize Summarizer
}

// Name implements harness.Hook.
func (c *Compactor) Name() string { return "compact" }

// BeforeModel compacts r.Messages (and the request) when needed.
func (c *Compactor) BeforeModel(ctx context.Context, r *harness.Run, req *model.Request) error {
	if c.MaxTokens <= 0 || model.EstimateTokens(r.Messages) <= c.MaxTokens {
		return nil
	}
	keep := c.KeepLast
	if keep <= 0 {
		keep = 6
	}
	cut := len(r.Messages) - keep
	for cut > 0 && r.Messages[cut].Role != model.RoleUser {
		cut--
	}
	if cut <= 0 {
		return nil
	}
	summarize := c.Summarize
	if summarize == nil {
		summarize = Digest
	}
	summary, err := summarize(ctx, r.Messages[:cut])
	if err != nil {
		return fmt.Errorf("compact: %w", err)
	}
	before := len(r.Messages)
	r.Messages = append([]model.Message{model.User("[Summary of the earlier conversation]\n" + summary)},
		r.Messages[cut:]...)
	r.Set("compact.last", fmt.Sprintf("%d → %d messages", before, len(r.Messages)))
	r.Note(c.Name(), "compacted %d → %d messages (~%d tokens over %d)", before, len(r.Messages),
		model.EstimateTokens(r.Messages), c.MaxTokens)
	// Rebuild the request history, keeping any leading system message.
	var sys []model.Message
	if len(req.Messages) > 0 && req.Messages[0].Role == model.RoleSystem {
		sys = req.Messages[:1]
	}
	req.Messages = append(append([]model.Message(nil), sys...), r.Messages...)
	return nil
}

// Digest is a model-free summariser: one line per message, clipped. It is
// deterministic and cheap, good for tests and as a fallback.
func Digest(_ context.Context, old []model.Message) (string, error) {
	var b strings.Builder
	for _, m := range old {
		text := m.Content
		if len(m.ToolCalls) > 0 {
			text = "called " + m.ToolCalls[0].Name
		}
		if len(text) > 80 {
			text = text[:80] + "…"
		}
		fmt.Fprintf(&b, "- %s: %s\n", m.Role, strings.ReplaceAll(text, "\n", " "))
	}
	return b.String(), nil
}

// ModelSummarizer asks a model to summarise.
func ModelSummarizer(p model.Provider, modelName string) Summarizer {
	return func(ctx context.Context, old []model.Message) (string, error) {
		var b strings.Builder
		for _, m := range old {
			fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
		}
		req := model.Request{Model: modelName, Messages: []model.Message{
			model.System("Summarise this conversation in a few bullet points. Keep facts, decisions and open questions."),
			model.User(b.String()),
		}}
		resp, err := model.Collect(p.Complete(ctx, req), nil)
		return resp.Text, err
	}
}
