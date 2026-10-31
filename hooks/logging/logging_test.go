package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/hooks/logging"
	"github.com/ecociel/agent-harness/model"
	"github.com/ecociel/agent-harness/provider/fake"
	"github.com/ecociel/agent-harness/tool"
)

func run(t *testing.T, level slog.Level, a *harness.Agent) string {
	t.Helper()
	var out bytes.Buffer
	l := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	_, _ = a.Run(harness.WithEvents(context.Background(), logging.Sink(l, level)), harness.Prompt("x"))
	return out.String()
}

func TestItLogsARun(t *testing.T) {
	noop := tool.Func(tool.Spec{Name: "noop"}, func(context.Context, json.RawMessage) (string, error) { return "", nil })
	got := run(t, slog.LevelDebug, &harness.Agent{Name: "demo", Model: "m1", Tools: tool.NewRegistry(noop),
		Provider: fake.Script(fake.Call("noop", nil), fake.Text("the answer"))})
	for _, want := range []string{"run.started", "model.done", "model=m1", "input_tokens=", "tool=noop", "tool.done", "run.ended", "agent=demo", "outcome=completed"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "msg=delta") || strings.Contains(got, "the answer") {
		t.Fatalf("streamed text reached the log:\n%s", got)
	}
	if strings.Contains(got, "level=WARN") {
		t.Fatalf("a clean run warned:\n%s", got)
	}
}

func TestTroubleIsAWarning(t *testing.T) {
	fails := fake.NewFunc(func(model.Request) (model.Response, error) {
		return model.Response{}, &model.Error{Class: model.ClassAuth, Err: errors.New("bad key")}
	})
	got := run(t, slog.LevelDebug, &harness.Agent{Provider: fails})
	if !strings.Contains(got, "level=WARN msg=model.failed") || !strings.Contains(got, "class=auth") || !strings.Contains(got, "level=WARN msg=run.ended") {
		t.Fatalf("%s", got)
	}
}
