package tool_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/ecociel/agent-harness/tool"
)

func echo(name string) tool.Tool {
	return tool.Func(tool.Spec{Name: name}, func(_ context.Context, a json.RawMessage) (string, error) {
		return name + ":" + string(a), nil
	})
}

func TestRegistry(t *testing.T) {
	r := tool.NewRegistry(echo("b"), echo("a"))
	if err := r.Add(echo("a")); err == nil {
		t.Fatal("duplicate accepted")
	}
	if got := r.Specs(); got[0].Name != "b" || got[1].Name != "a" {
		t.Fatalf("order = %v", got)
	}
	out, err := r.Call(context.Background(), "a", json.RawMessage(`{}`))
	if err != nil || out != "a:{}" {
		t.Fatalf("call = %q %v", out, err)
	}
	if _, err := r.Call(context.Background(), "zz", nil); !errors.Is(err, tool.ErrUnknown) {
		t.Fatalf("unknown err = %v", err)
	}
}

func TestHandoffUnwraps(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &tool.Handoff{Target: "billing"})
	h, ok := tool.AsHandoff(err)
	if !ok || h.Target != "billing" {
		t.Fatalf("got %v %v", h, ok)
	}
}
