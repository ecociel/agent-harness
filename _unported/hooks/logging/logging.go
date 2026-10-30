// Package logging writes the main plugin points (run, model, tool, transition)
// to a log/slog logger. It is the
// simplest way to watch a run in a terminal, and doubles as a reference for
// what each point receives.
package logging

import (
	"context"
	"log/slog"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
)

// Logger is the hook.
type Logger struct {
	L *slog.Logger
	// Level for routine points (default Debug); stops and denials use Warn.
	Level slog.Level
}

// New returns a hook logging to l at Debug.
func New(l *slog.Logger) *Logger { return &Logger{L: l, Level: slog.LevelDebug} }

// Name implements harness.Hook.
func (g *Logger) Name() string { return "logging" }

func (g *Logger) log(ctx context.Context, r *harness.Run, msg string, attrs ...any) {
	g.L.Log(ctx, g.Level, msg, append([]any{"run", r.ID, "agent", r.Agent.Name, "turn", r.Turn}, attrs...)...)
}

// BeforeRun logs the input.
func (g *Logger) BeforeRun(ctx context.Context, r *harness.Run) error {
	g.log(ctx, r, "run start", "input", model.LastUser(r.Messages))
	return nil
}

// BeforeModel logs the request size.
func (g *Logger) BeforeModel(ctx context.Context, r *harness.Run, req *model.Request) error {
	g.log(ctx, r, "model request", "messages", len(req.Messages), "tools", len(req.Tools))
	return nil
}

// AfterModel logs usage.
func (g *Logger) AfterModel(ctx context.Context, r *harness.Run, resp *model.Response) error {
	g.log(ctx, r, "model response", "finish", resp.Finish, "tool_calls", len(resp.ToolCalls),
		"input_tokens", resp.Usage.Input, "output_tokens", resp.Usage.Output)
	return nil
}

// AfterTool logs the outcome.
func (g *Logger) AfterTool(ctx context.Context, r *harness.Run, out harness.ToolOutcome) error {
	if out.Denied {
		g.L.WarnContext(ctx, "tool denied", "run", r.ID, "tool", out.Call.Name, "reason", out.Reason)
		return nil
	}
	g.log(ctx, r, "tool done", "tool", out.Call.Name, "duration", out.Duration, "err", out.Err)
	return nil
}

// OnTransition logs state changes.
func (g *Logger) OnTransition(ctx context.Context, r *harness.Run, from, to harness.State) {
	g.log(ctx, r, "transition", "from", from, "to", to)
}

// AfterRun logs the result.
func (g *Logger) AfterRun(ctx context.Context, r *harness.Run, res *harness.Result) {
	level := g.Level
	if res.Stop != harness.StopCompleted {
		level = slog.LevelWarn
	}
	g.L.Log(ctx, level, "run end", "run", r.ID, "stop", res.Stop, "detail", res.Detail,
		"turns", res.Turns, "tokens", res.Usage.Total(), "duration", res.Duration)
}
