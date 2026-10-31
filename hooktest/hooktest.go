// Package hooktest checks a rule the compiler cannot: a hook returns values and leaves
// its Run alone. A hook gets a Run by value, but its slices are shared with the machine
// to keep a call cheap, so a hook that writes through one changes a live run behind
// the journal's back. Production does not check this, so tests must.
//
//	agent.Hooks = hooktest.Frozen(t, hooks...)
package hooktest

import (
	"context"
	"encoding/json"
	"iter"
	"testing"

	"github.com/ecociel/agent-harness"
	"github.com/ecociel/agent-harness/model"
)

// Frozen returns hooks that behave like the ones given and fail t if any changes the
// Run it was handed. A Redactor is returned as it is, because the machine treats one
// differently and it is never given a Run to change, except in WrapModel.
func Frozen(t testing.TB, hooks ...harness.Hook) []harness.Hook {
	out := make([]harness.Hook, len(hooks))
	for i, h := range hooks {
		out[i] = h
		if _, ok := h.(harness.Redactor); !ok {
			out[i] = &frozen{t: t, h: h}
		}
	}
	return out
}

type frozen struct {
	t testing.TB
	h harness.Hook
}

func (f *frozen) Name() string { return f.h.Name() }

// print is everything a hook could reach. Events carry no JSON tag on Run, so they
// are printed apart.
func print(r harness.Run) string {
	run, _ := json.Marshal(r)
	events, _ := json.Marshal(r.Events)
	return string(run) + string(events)
}

// guard runs body and reports a change to r.
func (f *frozen) guard(point harness.Point, r harness.Run, body func()) {
	before := print(r)
	body()
	if print(r) != before {
		f.t.Errorf("hook %s changed its Run in %s", f.h.Name(), point)
	}
}

func (f *frozen) BeforeRun(ctx context.Context, r harness.Run) (err error) {
	if h, ok := f.h.(harness.BeforeRunHook); ok {
		f.guard(harness.PointBeforeRun, r, func() { err = h.BeforeRun(ctx, r) })
	}
	return err
}

func (f *frozen) Context(ctx context.Context, r harness.Run) (e harness.Edit, err error) {
	if h, ok := f.h.(harness.ContextHook); ok {
		f.guard(harness.PointContext, r, func() { e, err = h.Context(ctx, r) })
	}
	return e, err
}

func (f *frozen) BeforeModel(ctx context.Context, r harness.Run, req *model.Request) (err error) {
	if h, ok := f.h.(harness.BeforeModelHook); ok {
		f.guard(harness.PointBeforeModel, r, func() { err = h.BeforeModel(ctx, r, req) })
	}
	return err
}

func (f *frozen) WrapModel(r harness.Run, next model.Provider) model.Provider {
	h, ok := f.h.(harness.ModelMiddleware)
	if !ok {
		return next
	}
	p := h.WrapModel(r, next)
	return model.ProviderFunc(func(ctx context.Context, req model.Request) iter.Seq2[model.Event, error] {
		return func(yield func(model.Event, error) bool) {
			f.guard(harness.PointWrapModel, r, func() { p.Complete(ctx, req)(yield) })
		}
	})
}

func (f *frozen) AfterModel(ctx context.Context, r harness.Run, resp model.Response) (out model.Response, err error) {
	out = resp
	if h, ok := f.h.(harness.AfterModelHook); ok {
		f.guard(harness.PointAfterModel, r, func() { out, err = h.AfterModel(ctx, r, resp) })
	}
	return out, err
}

func (f *frozen) Recover(ctx context.Context, r harness.Run, fail harness.Failure) (rec harness.Recovery) {
	if h, ok := f.h.(harness.RecoverHook); ok {
		f.guard(harness.PointRecover, r, func() { rec = h.Recover(ctx, r, fail) })
	}
	return rec
}

func (f *frozen) BeforeTool(ctx context.Context, r harness.Run, c harness.Call) (d harness.Decision) {
	if h, ok := f.h.(harness.BeforeToolHook); ok {
		f.guard(harness.PointBeforeTool, r, func() { d = h.BeforeTool(ctx, r, c) })
	}
	return d
}

func (f *frozen) WrapTool(r harness.Run, next harness.ToolFunc) harness.ToolFunc {
	h, ok := f.h.(harness.ToolMiddleware)
	if !ok {
		return next
	}
	wrapped := h.WrapTool(r, next)
	return func(ctx context.Context, c harness.Call) (out string, err error) {
		f.guard(harness.PointWrapTool, r, func() { out, err = wrapped(ctx, c) })
		return out, err
	}
}

func (f *frozen) AfterTool(ctx context.Context, r harness.Run, c harness.Call, content string) (out string, err error) {
	out = content
	if h, ok := f.h.(harness.AfterToolHook); ok {
		f.guard(harness.PointAfterTool, r, func() { out, err = h.AfterTool(ctx, r, c, content) })
	}
	return out, err
}

func (f *frozen) CheckOutput(ctx context.Context, r harness.Run, output string) (v harness.Verdict) {
	if h, ok := f.h.(harness.OutputHook); ok {
		f.guard(harness.PointCheckOutput, r, func() { v = h.CheckOutput(ctx, r, output) })
	}
	return v
}

func (f *frozen) AfterRun(ctx context.Context, r harness.Run) {
	if h, ok := f.h.(harness.AfterRunHook); ok {
		f.guard(harness.PointAfterRun, r, func() { h.AfterRun(ctx, r) })
	}
}
