// Package demo is what the hook examples share: the model choice, a plain-text
// timeline of a run, and a Run that prints where the run ended up.
package demo

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"strings"
	"sync/atomic"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/examples/internal/llm"
	"github.com/ecociel/agent-harness/model"
)

const mockName = "mock"

// Provider returns the provider HARNESS_PROVIDER asks for, or mock if it asks for
// none, with the model name to put on the Agent.
func Provider(mock model.Provider) (model.Provider, string) {
	p, name, label, err := llm.FromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if p == nil {
		fmt.Println("model: " + mockName + " (set HARNESS_PROVIDER for a real one)")
		return mock, mockName
	}
	fmt.Println("model: " + label)
	return p, name
}

// Context returns a context whose runs print the timeline.
func Context() context.Context {
	return harness.WithEvents(context.Background(), timeline)
}

var runs atomic.Int64

// Run prints the input, advances the run and prints how it came out. An input with
// no RunID gets the next "run-N", so the output is the same every time.
func Run(ctx context.Context, a *harness.Agent, in harness.Input) *harness.Run {
	if in.RunID == "" {
		in.RunID = fmt.Sprintf("run-%d", runs.Add(1))
	}
	fmt.Println()
	if len(in.Signals) > 0 {
		for _, s := range in.Signals {
			fmt.Printf("> resume %s: %q from %s\n", in.RunID, s.Value, s.By)
		}
	} else {
		fmt.Printf("> %s\n", clip(in.Text()))
	}
	res, err := a.Run(ctx, in)
	if err != nil {
		fmt.Println("run failed:", err)
		os.Exit(1)
	}
	if res.Output != "" {
		fmt.Printf("< %s\n", clip(res.Output))
	}
	fmt.Println("stop:", res.Stop)
	fmt.Println("path:", strings.Trim(fmt.Sprint(res.Path), "[]"))
	return res
}

// FailFirst fails the first n calls to p with class. It wraps whichever provider was
// chosen, so an example that injects a fault behaves the same on a real model.
func FailFirst(p model.Provider, n int, class model.ErrorClass) model.Provider {
	var calls atomic.Int64
	return model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
		if calls.Add(1) <= int64(n) {
			return model.Fail(&model.Error{Class: class, Err: errors.New("injected failure")})
		}
		return p.Complete(ctx, req)
	})
}

// timeline is the sink behind Context. It leaves out times, durations and backoffs, so
// a mock run prints the same text every time.
func timeline(e harness.Event) {
	if line := describe(e); line != "" {
		fmt.Println("  " + line)
	}
}

func describe(e harness.Event) string {
	switch e.Kind {
	case harness.KindRunStarted:
		return fmt.Sprintf("%s started, agent %s", e.RunID, e.Agent)
	case harness.KindRunClaimed:
		return "worker claimed the run"
	case harness.KindModelStarted:
		return call(e)
	case harness.KindModelDone:
		return reply(*e.Reply)
	case harness.KindModelFailed:
		return failure(e)
	case harness.KindModelLost:
		return "model call lost"
	case harness.KindToolGated:
		return gate(e)
	case harness.KindToolDone:
		return fmt.Sprintf("tool %s returned: %s", e.Tool, clip(e.Text))
	case harness.KindToolLost:
		return fmt.Sprintf("tool %s lost", e.Tool)
	case harness.KindHistoryEdited:
		return fmt.Sprintf("history edited by %s: messages [%d:%d] replaced with %d new, source %q",
			e.Hook, e.Edit.From, e.Edit.To, len(e.Edit.With), e.Edit.Source)
	case harness.KindOutputChecked:
		return verdict(e)
	case harness.KindWaitResolved:
		return fmt.Sprintf("wait resolved: %s, %q from %s", e.Signal.Kind, e.Signal.Value, e.Signal.By)
	case harness.KindRunEnded:
		return "run ended: " + joined(e.Outcome, e.Text)
	case harness.KindHook:
		return hook(e)
	}
	return ""
}

// call numbers the turn the way a reader counts. An event carries the Run as it stood
// before the event, so a first attempt says the turn before it, and a retry its own.
func call(e harness.Event) string {
	turn, attempt := e.Turn, ""
	if e.Attempt == 1 {
		turn++
	} else {
		attempt = fmt.Sprintf(", attempt %d", e.Attempt)
	}
	tools := "no tools"
	if len(e.Offered) > 0 {
		tools = "tools " + strings.Join(e.Offered, " ")
	}
	return fmt.Sprintf("model call: turn %d%s, %s", turn, attempt, tools)
}

func reply(r model.Response) string {
	if len(r.ToolCalls) == 0 {
		return "model replied: " + clip(r.Text)
	}
	calls := make([]string, len(r.ToolCalls))
	for i, c := range r.ToolCalls {
		calls[i] = c.Name + " " + string(c.Arguments)
	}
	return "model asked for: " + clip(strings.Join(calls, ", "))
}

func failure(e harness.Event) string {
	if e.Stop != "" {
		return "model call stopped: " + joined(string(e.Stop), e.Text)
	}
	line := fmt.Sprintf("model failed: %s, attempt %d", e.Failure.Class, e.Attempt)
	switch {
	case e.Recovery == nil || !e.Recovery.Retry:
		return line + ", no retry"
	case e.Recovery.Model != "":
		return line + ", retry on " + e.Recovery.Model + waits(e)
	}
	return line + ", retry" + waits(e)
}

func gate(e harness.Event) string {
	what := e.Outcome
	if e.Hook != "" {
		what += " by " + e.Hook
	}
	return fmt.Sprintf("tool %s gate: %s", e.Tool, joined(what, e.Text)) + waits(e)
}

func waits(e harness.Event) string {
	var out string
	for _, w := range e.Waits {
		out += ", wait opened on " + joined(w.Kind, w.Detail)
	}
	return out
}

func verdict(e harness.Event) string {
	switch {
	case e.Verdict.Action != harness.VerdictAccept:
		return fmt.Sprintf("output %s by %s: %s", e.Verdict.Action, e.Hook, clip(e.Verdict.Feedback))
	case e.Verdict.Rewrite != "":
		return "output accepted, rewritten: " + clip(e.Verdict.Rewrite)
	}
	return "output accepted"
}

// hook keeps the invocations that add something. A gate or a verdict is left out because
// the recorded line after it says the same and names the hook, and one that returned "ok"
// with no note did nothing to see.
func hook(e harness.Event) string {
	if e.Point == harness.PointBeforeTool || e.Point == harness.PointCheckOutput || e.Outcome == "ok" && e.Text == "" {
		return ""
	}
	return fmt.Sprintf("hook %s %s: %s", e.Hook, e.Point, joined(e.Outcome, e.Text))
}

func joined(what, why string) string {
	if why == "" {
		return what
	}
	return what + " (" + clip(why) + ")"
}

const maxText = 100

func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxText {
		return string(r[:maxText]) + "..."
	}
	return s
}
