// Package compact keeps the conversation inside a context budget by summarising old
// turns before the model call.
//
// When the estimated history exceeds MaxTokens, or the provider reports that the
// context is full, the oldest part is replaced by one summary message and about the
// last KeepLast messages stay as they are. The edit is recorded, so a summary is
// computed once and survives a crash, and it lasts for the rest of the run. The cut
// never falls inside a tool round, so a tool call keeps its result.
//
// Replacing old turns leaves provider state on the turns kept behind them stale, so
// the core drops it: on Anthropic those turns go back without their thinking.
//
// A summariser that calls a model pays for it outside the run: that spend is not in
// Run.Usage, and a crash before the edit is recorded pays it again.
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
	// KeepLast messages are kept verbatim (default 6). A tool round on the cut stays
	// whole, so a few more can stay.
	KeepLast int
	// Summarize builds the summary; nil uses Digest.
	Summarize Summarizer
}

// Name implements harness.Hook.
func (c *Compactor) Name() string { return "compact" }

// Context implements harness.ContextHook. A summariser that fails leaves the history
// alone: the model may still have room, and if not the run fails on what the provider
// says.
func (c *Compactor) Context(ctx context.Context, r harness.Run) (harness.Edit, error) {
	full := r.Failure != nil && r.Failure.Class == model.ClassContextLength
	if !full && (c.MaxTokens <= 0 || model.EstimateTokens(r.Messages) <= c.MaxTokens) {
		return harness.Edit{}, nil
	}
	keep := c.KeepLast
	if keep <= 0 {
		keep = 6
	}
	cut := len(r.Messages) - keep
	if full {
		cut = max(cut, len(r.Messages)/2) // the estimate was wrong, so take at least half
	}
	if len(r.Pinned) > 0 && r.Inherited > 0 && cut > r.Inherited {
		cut = r.Inherited // an edit may not cross the pinned context
	}
	for cut > 0 && cut < len(r.Messages) && r.Messages[cut].Role == model.RoleTool {
		cut--
	}
	if cut < 2 {
		return harness.Edit{}, nil // one message has nothing to gain from a summary
	}
	summarize := c.Summarize
	if summarize == nil {
		summarize = Digest
	}
	summary, err := summarize(ctx, r.Messages[:cut])
	if err != nil {
		harness.Note(ctx, "summary failed, history kept: %v", err)
		return harness.Edit{}, nil
	}
	harness.Note(ctx, "%d of %d messages summarised", cut, len(r.Messages))
	return harness.Edit{To: cut, Source: c.Name(),
		With: []model.Message{model.User("[Summary of the earlier conversation]\n" + summary)}}, nil
}

// Recover implements harness.RecoverHook: a full context gets one more try, with the
// compaction that Context does when it sees this failure.
func (c *Compactor) Recover(_ context.Context, _ harness.Run, f harness.Failure) harness.Recovery {
	return harness.Recovery{Retry: f.Class == model.ClassContextLength && f.Attempt == 1}
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
			text = strings.ToValidUTF8(text[:80], "") + "…"
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
