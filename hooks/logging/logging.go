// Package logging writes a run's events to a log/slog logger. It is the simplest way
// to watch a run in a terminal.
//
// It is a sink, not a hook, like trace: attach it once on the context and every run in
// the tree reports to it, once per event. Only what has passed the redactors reaches a
// sink, so a log holds nothing a journal would not.
package logging

import (
	"context"
	"log/slog"

	"github.com/ecociel/agent-harness"
)

// Sink returns an event sink logging to l at level, or at Warn for a failure, a denial
// or a run that stopped early. Streamed text is left out. Pass it to harness.WithEvents.
func Sink(l *slog.Logger, level slog.Level) harness.EventSink {
	return func(e harness.Event) {
		if e.Kind == harness.KindDelta {
			return
		}
		attrs := []any{"run", e.RunID, "agent", e.Agent, "turn", e.Turn}
		add := func(key string, value any, set bool) {
			if set {
				attrs = append(attrs, key, value)
			}
		}
		add("from", e.From, e.From != "")
		add("to", e.To, e.To != "")
		add("model", e.Model, e.Model != "")
		add("tool", e.Tool, e.Tool != "")
		add("hook", e.Hook, e.Hook != "")
		add("point", e.Point, e.Point != "")
		add("outcome", e.Outcome, e.Outcome != "")
		add("stop", e.Stop, e.Stop != "")
		add("duration", e.Duration, e.Duration != 0)
		add("attempt", e.Attempt, e.Attempt != 0)
		if e.Reply != nil {
			attrs = append(attrs, "finish", e.Reply.Finish, "tool_calls", len(e.Reply.ToolCalls),
				"input_tokens", e.Reply.Usage.Input, "output_tokens", e.Reply.Usage.Output)
		}
		if e.Failure != nil {
			attrs = append(attrs, "class", e.Failure.Class, "detail", e.Failure.Detail)
		}
		l.Log(context.Background(), levelOf(e, level), string(e.Kind), attrs...)
	}
}

func levelOf(e harness.Event, level slog.Level) slog.Level {
	stopped := e.Kind == harness.KindRunStopping && e.Stop != harness.StopCompleted ||
		e.Kind == harness.KindRunEnded && e.Outcome != string(harness.StopCompleted)
	denied := e.Decision != nil && e.Decision.Action != harness.ActionAllow
	if stopped || denied || e.Failure != nil || e.Kind == harness.KindModelLost || e.Kind == harness.KindToolLost {
		return slog.LevelWarn
	}
	return level
}
